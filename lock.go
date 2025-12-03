package etcdlock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-lynx/lynx/app/log"
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

// GetExpiration gets the lock expiration time
func (el *EtcdLock) GetExpiration() time.Duration {
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

// Renew manually renews the lock
func (el *EtcdLock) Renew(ctx context.Context, newExpiration time.Duration) error {
	el.mutex.Lock()
	defer el.mutex.Unlock()

	if el.leaseID == 0 {
		return ErrLockNotHeld
	}

	// Create new lease with new expiration
	lease, err := el.client.Grant(ctx, int64(newExpiration.Seconds()))
	if err != nil {
		globalCallback.OnLockRenewalFailed(el.key, err)
		return fmt.Errorf("failed to grant lease: %w", err)
	}

	// Keep alive the new lease
	_, err = el.client.KeepAliveOnce(ctx, lease.ID)
	if err != nil {
		globalCallback.OnLockRenewalFailed(el.key, err)
		return fmt.Errorf("failed to keep alive lease: %w", err)
	}

	// Update lease ID and expiration
	el.leaseID = lease.ID
	el.expiration = newExpiration
	el.expiresAt = time.Now().Add(newExpiration)

	globalCallback.OnLockRenewed(el.key, newExpiration)
	return nil
}

// Release releases the lock
func (el *EtcdLock) Release(ctx context.Context) error {
	el.mutex.Lock()
	defer el.mutex.Unlock()

	if el.leaseID == 0 {
		return ErrLockNotHeld
	}

	// Revoke the lease to release the lock
	_, err := el.client.Revoke(ctx, el.leaseID)
	if err != nil {
		return fmt.Errorf("failed to revoke lease: %w", err)
	}

	duration := time.Since(el.acquiredAt)
	globalCallback.OnLockReleased(el.key, duration)

	// Reset lease ID
	el.leaseID = 0

	return nil
}

// IsLocked checks if the lock is held by the current instance
func (el *EtcdLock) IsLocked(ctx context.Context) (bool, error) {
	el.mutex.Lock()
	leaseID := el.leaseID
	el.mutex.Unlock()

	if leaseID == 0 {
		return false, nil
	}

	// Check if lease still exists
	ttlResp, err := el.client.TimeToLive(ctx, leaseID)
	if err != nil {
		return false, err
	}

	return ttlResp.TTL > 0, nil
}

// Acquire attempts to acquire the lock
func (el *EtcdLock) Acquire(ctx context.Context) error {
	lockKey := buildLockKey(el.key)

	// Create lease
	lease, err := el.client.Grant(ctx, int64(el.expiration.Seconds()))
	if err != nil {
		globalCallback.OnLockAcquireFailed(el.key, err)
		return fmt.Errorf("failed to grant lease: %w", err)
	}

	// Try to acquire lock with transaction
	txn := el.client.Txn(ctx)
	txn.If(clientv3.Compare(clientv3.CreateRevision(lockKey), "=", 0)).
		Then(clientv3.OpPut(lockKey, "", clientv3.WithLease(lease.ID))).
		Else(clientv3.OpGet(lockKey))

	txnResp, err := txn.Commit()
	if err != nil {
		el.client.Revoke(context.Background(), lease.ID)
		globalCallback.OnLockAcquireFailed(el.key, err)
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	if !txnResp.Succeeded {
		el.client.Revoke(context.Background(), lease.ID)
		globalCallback.OnLockAcquireFailed(el.key, ErrLockAcquireConflict)
		return ErrLockAcquireConflict
	}

	// Lock acquired successfully
	now := time.Now()
	el.mutex.Lock()
	el.leaseID = lease.ID
	el.acquiredAt = now
	el.expiresAt = now.Add(el.expiration)
	el.mutex.Unlock()

	// Start keep-alive if renewal is enabled
	if el.renewalThreshold > 0 {
		el.ctx, el.cancel = context.WithCancel(context.Background())
		go el.keepAlive()
	}

	globalCallback.OnLockAcquired(el.key, el.expiration)
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
				select {
				case <-time.After(delay):
				case <-ctx.Done():
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
	ch, kaErr := el.client.KeepAlive(el.ctx, el.leaseID)
	if kaErr != nil {
		log.ErrorCtx(el.ctx, "failed to start keep alive", "error", kaErr)
		return
	}

	for {
		select {
		case <-el.ctx.Done():
			return
		case ka, ok := <-ch:
			if !ok {
				log.WarnCtx(el.ctx, "keep alive channel closed", "key", el.key)
				return
			}
			if ka != nil {
				el.mutex.Lock()
				el.expiresAt = time.Now().Add(time.Duration(ka.TTL) * time.Second)
				el.mutex.Unlock()
			}
		}
	}
}

// NewLock creates a reusable lock instance
func NewLock(ctx context.Context, client *clientv3.Client, key string, options LockOptions) (*EtcdLock, error) {
	// Validate lock key name
	if err := ValidateKey(key); err != nil {
		return nil, fmt.Errorf("invalid lock key: %w", err)
	}
	// Validate configuration options
	if err := options.Validate(); err != nil {
		return nil, fmt.Errorf("invalid lock options: %w", err)
	}

	// Create lock instance
	lock := &EtcdLock{
		client:           client,
		key:              key,
		expiration:       options.Expiration,
		renewalThreshold: options.RenewalThreshold,
	}
	return lock, nil
}
