package etcdlock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-lynx/lynx/log"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var (
	// ErrLockAcquireConflict indicates lock is already held by another process
	ErrLockAcquireConflict = errors.New("lock acquire conflict")
	// ErrLockNotHeld indicates lock is not held by current process
	ErrLockNotHeld = errors.New("lock not held")
	// ErrLockRenewalFailed indicates lock renewal failed
	ErrLockRenewalFailed = errors.New("lock renewal failed")
	// ErrMaxRetriesExceeded indicates maximum retries exceeded
	ErrMaxRetriesExceeded = errors.New("max retries exceeded")
	// ErrLockFnRequired indicates callback function is required
	ErrLockFnRequired = errors.New("lock callback function is required")
)

// GetKey gets the lock key name
func (el *EtcdLock) GetKey() string {
	return el.key
}

func (el *EtcdLock) currentClient(ctx context.Context) (*clientv3.Client, error) {
	if el == nil {
		return nil, fmt.Errorf("etcd lock is nil")
	}
	if el.provider == nil {
		return nil, fmt.Errorf("etcd client provider not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := el.provider.Client(ctx)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("etcd client is nil")
	}
	return client, nil
}

// GetExpiration gets the lock expiration time
func (el *EtcdLock) GetExpiration() time.Duration {
	el.mutex.Lock()
	defer el.mutex.Unlock()
	return el.expiration
}

// GetExpiresAt gets the lock expiration time point
func (el *EtcdLock) GetExpiresAt() time.Time {
	el.mutex.Lock()
	defer el.mutex.Unlock()
	return el.expiresAt
}

// GetAcquiredAt gets the lock acquisition time
func (el *EtcdLock) GetAcquiredAt() time.Time {
	el.mutex.Lock()
	defer el.mutex.Unlock()
	return el.acquiredAt
}

// GetRemainingTime gets the remaining time of the lock
func (el *EtcdLock) GetRemainingTime() time.Duration {
	el.mutex.Lock()
	defer el.mutex.Unlock()
	return time.Until(el.expiresAt)
}

// IsExpired checks if the lock has expired
func (el *EtcdLock) IsExpired() bool {
	el.mutex.Lock()
	defer el.mutex.Unlock()
	return time.Now().After(el.expiresAt)
}

// Renew manually renews the lock by extending the existing lease via KeepAliveOnce.
func (el *EtcdLock) Renew(ctx context.Context, newExpiration time.Duration) (renewErr error) {
	client, err := el.currentClient(ctx)
	if err != nil {
		currentCallback().OnLockRenewalFailed(el.key, err)
		return err
	}
	start := time.Now()
	defer func() {
		observeOperationLatency("renew", operationStatus(renewErr), time.Since(start))
	}()

	el.mutex.Lock()
	leaseID := el.leaseID
	el.mutex.Unlock()

	if leaseID == 0 {
		return ErrLockNotHeld
	}
	if newExpiration > 0 && newExpiration != el.expiration {
		err := fmt.Errorf("etcd lease ttl cannot be changed during renewal: current=%v requested=%v", el.expiration, newExpiration)
		currentCallback().OnLockRenewalFailed(el.key, err)
		return err
	}

	// Extend the existing lease (key is bound to this lease)
	resp, err := client.KeepAliveOnce(ctx, leaseID)
	if err != nil {
		currentCallback().OnLockRenewalFailed(el.key, err)
		return fmt.Errorf("failed to keep alive lease: %w", err)
	}

	el.mutex.Lock()
	renewedFor := el.expiration
	if resp != nil && resp.TTL > 0 {
		renewedFor = time.Duration(resp.TTL) * time.Second
		el.expiresAt = time.Now().Add(renewedFor)
	} else {
		el.expiresAt = time.Now().Add(el.expiration)
	}
	el.mutex.Unlock()

	currentCallback().OnLockRenewed(el.key, renewedFor)
	return nil
}

// Release releases the lock
func (el *EtcdLock) Release(ctx context.Context) (releaseErr error) {
	client, err := el.currentClient(ctx)
	if err != nil {
		return err
	}
	start := time.Now()
	defer func() {
		observeOperationLatency("unlock", operationStatus(releaseErr), time.Since(start))
	}()

	el.mutex.Lock()
	leaseID := el.leaseID
	cancel := el.cancel
	el.mutex.Unlock()

	if leaseID == 0 {
		return ErrLockNotHeld
	}

	// Revoke the lease to release the lock
	_, err = client.Revoke(ctx, leaseID)
	if err != nil {
		return fmt.Errorf("failed to revoke lease: %w", err)
	}

	if cancel != nil {
		cancel()
	}
	globalLockManager.removeLock(el)

	el.mutex.Lock()
	duration := time.Since(el.acquiredAt)
	el.leaseID = 0
	el.cancel = nil
	el.ctx = nil
	el.mutex.Unlock()

	currentCallback().OnLockReleased(el.key, duration)
	return nil
}

// IsLocked checks if the lock is held by the current instance
func (el *EtcdLock) IsLocked(ctx context.Context) (bool, error) {
	client, err := el.currentClient(ctx)
	if err != nil {
		return false, err
	}
	el.mutex.Lock()
	leaseID := el.leaseID
	el.mutex.Unlock()

	if leaseID == 0 {
		return false, nil
	}

	// Check if lease still exists
	ttlResp, err := client.TimeToLive(ctx, leaseID)
	if err != nil {
		return false, err
	}

	return ttlResp.TTL > 0, nil
}

// Acquire attempts to acquire the lock
func (el *EtcdLock) Acquire(ctx context.Context) (acquireErr error) {
	client, err := el.currentClient(ctx)
	if err != nil {
		currentCallback().OnLockAcquireFailed(el.key, err)
		return err
	}
	start := time.Now()
	defer func() {
		observeOperationLatency("lock", operationStatus(acquireErr), time.Since(start))
	}()

	lockKey := buildLockKey(el.key)

	// Create lease
	lease, err := client.Grant(ctx, int64(el.expiration.Seconds()))
	if err != nil {
		currentCallback().OnLockAcquireFailed(el.key, err)
		return fmt.Errorf("failed to grant lease: %w", err)
	}

	// Try to acquire lock with transaction
	txn := client.Txn(ctx)
	txn.If(clientv3.Compare(clientv3.CreateRevision(lockKey), "=", 0)).
		Then(clientv3.OpPut(lockKey, "", clientv3.WithLease(lease.ID))).
		Else(clientv3.OpGet(lockKey))

	txnResp, err := txn.Commit()
	if err != nil {
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		client.Revoke(revokeCtx, lease.ID)
		revokeCancel()
		currentCallback().OnLockAcquireFailed(el.key, err)
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	if !txnResp.Succeeded {
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		client.Revoke(revokeCtx, lease.ID)
		revokeCancel()
		currentCallback().OnLockAcquireFailed(el.key, ErrLockAcquireConflict)
		return ErrLockAcquireConflict
	}

	// Lock acquired successfully
	now := time.Now()
	el.mutex.Lock()
	el.leaseID = lease.ID
	el.acquiredAt = now
	el.expiresAt = now.Add(el.expiration)
	renewalEnabled := el.renewalEnabled
	renewalThreshold := el.renewalThreshold
	el.mutex.Unlock()

	// Start keep-alive if renewal is enabled
	if renewalEnabled && renewalThreshold > 0 {
		el.mutex.Lock()
		el.ctx, el.cancel = context.WithCancel(context.Background())
		el.mutex.Unlock()
		go el.keepAlive()
	}

	currentCallback().OnLockAcquired(el.key, el.expiration)
	return nil
}

// AcquireWithRetry acquires the lock and retries according to strategy
func (el *EtcdLock) AcquireWithRetry(ctx context.Context, strategy RetryStrategy) error {
	retries := 0
	for {
		if strategy.MaxRetries > 0 && retries >= strategy.MaxRetries {
			return ErrMaxRetriesExceeded
		}
		if retries > 0 {
			// Add jitter to avoid hot spot collisions
			delay := strategy.RetryDelay
			if delay > 0 {
				if !waitForRetryDelay(ctx, delay) {
					return ctx.Err()
				}
			}
		}
		err := el.Acquire(ctx)
		if err == nil {
			return nil
		}
		if err != ErrLockAcquireConflict {
			return err
		}
		// Continue retrying according to strategy on conflict
		if strategy.MaxRetries == 0 {
			return ErrLockAcquireConflict
		}
		retries++
	}
}

// keepAlive keeps the lease alive
func (el *EtcdLock) keepAlive() {
	for {
		el.mutex.Lock()
		ctx := el.ctx
		leaseID := el.leaseID
		el.mutex.Unlock()
		if ctx == nil || leaseID == 0 {
			return
		}

		client, err := el.currentClient(ctx)
		if err != nil {
			log.ErrorCtx(ctx, "failed to resolve etcd client for keep alive", "error", err)
			return
		}
		ch, kaErr := client.KeepAlive(ctx, leaseID)
		if kaErr != nil {
			log.ErrorCtx(ctx, "failed to start keep alive", "error", kaErr)
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case ka, ok := <-ch:
				if !ok {
					log.WarnCtx(ctx, "keep alive channel closed", "key", el.key)
					if !waitForRetryDelay(ctx, 50*time.Millisecond) {
						return
					}
					goto retryKeepAlive
				}
				if ka != nil {
					el.mutex.Lock()
					el.expiresAt = time.Now().Add(time.Duration(ka.TTL) * time.Second)
					el.mutex.Unlock()
				}
			}
		}
	retryKeepAlive:
	}
}

// NewLock creates a reusable lock instance
func NewLock(ctx context.Context, provider ClientProvider, key string, options LockOptions) (*EtcdLock, error) {
	// Validate lock key name
	if err := ValidateKey(key); err != nil {
		return nil, fmt.Errorf("invalid lock key: %w", err)
	}
	options = normalizeLockOptions(options)
	// Validate configuration options
	if err := options.Validate(); err != nil {
		return nil, fmt.Errorf("invalid lock options: %w", err)
	}
	if provider == nil {
		return nil, fmt.Errorf("etcd client provider not found")
	}
	if _, err := provider.Client(ctx); err != nil {
		return nil, fmt.Errorf("failed to resolve etcd client: %w", err)
	}

	// Create lock instance
	lock := &EtcdLock{
		provider:         provider,
		key:              key,
		expiration:       options.Expiration,
		renewalThreshold: options.RenewalThreshold,
		renewalEnabled:   options.RenewalEnabled,
	}
	return lock, nil
}
