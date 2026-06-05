package etcdlock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-lynx/lynx/log"
	"github.com/go-lynx/lynx/pkg/timex"
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
	// ErrLockAlreadyHeld indicates Acquire was called on a lock that is already held
	ErrLockAlreadyHeld = errors.New("lock already held by this instance")
	// ErrLockLost indicates the lock was permanently lost (lease expired / renewal failed)
	ErrLockLost = errors.New("lock lost")
)

// leaseTTLSeconds converts a lock expiration into the integer-second TTL etcd
// leases require. etcd lease granularity is whole seconds, so any sub-second
// component is rounded UP to avoid the lease expiring before the renewal math
// (which uses the full duration) fires.
func leaseTTLSeconds(expiration time.Duration) int64 {
	secs := expiration / time.Second
	if expiration%time.Second != 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	return int64(secs)
}

// revokeLease best-effort revokes a lease, retrying a few times. A failed
// revoke leaves the lock held until the lease TTL elapses, so the failure is
// logged rather than silently ignored. Returns the last revoke error (nil on
// success) so callers can decide how to react.
func revokeLease(client *clientv3.Client, leaseID clientv3.LeaseID, key string) error {
	if client == nil || leaseID == 0 {
		return nil
	}
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, lastErr = client.Revoke(revokeCtx, leaseID)
		revokeCancel()
		if lastErr == nil {
			return nil
		}
		log.WarnCtx(context.Background(), "failed to revoke etcd lease",
			"key", key, "lease", int64(leaseID), "attempt", i+1, "error", lastErr)
		if i < attempts-1 {
			waitForRetryDelay(context.Background(), 100*time.Millisecond)
		}
	}
	log.ErrorCtx(context.Background(), "etcd lease revoke failed after retries; lock held until TTL",
		"key", key, "lease", int64(leaseID), "error", lastErr)
	return lastErr
}

// GetKey returns the resource key this lock guards.
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
	el.mutex.RLock()
	defer el.mutex.RUnlock()
	return el.expiration
}

// GetExpiresAt gets the lock expiration time point
func (el *EtcdLock) GetExpiresAt() time.Time {
	el.mutex.RLock()
	defer el.mutex.RUnlock()
	return el.expiresAt
}

// GetAcquiredAt gets the lock acquisition time
func (el *EtcdLock) GetAcquiredAt() time.Time {
	el.mutex.RLock()
	defer el.mutex.RUnlock()
	return el.acquiredAt
}

// GetRemainingTime gets the remaining time of the lock
func (el *EtcdLock) GetRemainingTime() time.Duration {
	el.mutex.RLock()
	defer el.mutex.RUnlock()
	return time.Until(el.expiresAt)
}

// IsExpired checks if the lock has expired
func (el *EtcdLock) IsExpired() bool {
	el.mutex.RLock()
	defer el.mutex.RUnlock()
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

	el.mutex.RLock()
	leaseID := el.leaseID
	el.mutex.RUnlock()

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
	lost := el.lostErr != nil
	el.mutex.Unlock()

	if leaseID == 0 {
		// If the lock was permanently lost, markLost already zeroed leaseID and
		// dropped the lease (etcd auto-deletes the key). Report the loss rather
		// than ErrLockNotHeld so the deferred Release in LockWithOptions does not
		// add ErrLockNotHeld noise alongside the ErrLockLost the caller already
		// gets. Callers still detect the loss via errors.Is(err, ErrLockLost),
		// and this never reports a clean release.
		if lost {
			return ErrLockLost
		}
		return ErrLockNotHeld
	}

	// Revoke the lease to release the lock. Stop the keepAlive goroutine first
	// so it cannot race the revoke or keep extending the lease we are dropping.
	if cancel != nil {
		cancel()
	}

	if _, err = client.Revoke(ctx, leaseID); err != nil {
		// Do NOT zero leaseID on failure: the lock is still held (until TTL),
		// so the caller must be able to retry Release. Retry in the background
		// to best-effort drop the lease rather than waiting the full TTL.
		// On eventual success, clean up in-process state so the manager stops
		// renewing and future Release calls return ErrLockNotHeld rather than
		// looping forever.
		go func() {
			if revokeLease(client, leaseID, el.key) == nil {
				globalLockManager.removeLock(el)
				el.mutex.Lock()
				if el.leaseID == leaseID { // guard against a concurrent re-acquire on the same instance
					el.leaseID = 0
					el.cancel = nil
					el.ctx = nil
				}
				el.mutex.Unlock()
			}
		}()
		return fmt.Errorf("failed to revoke lease: %w", err)
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
	el.mutex.RLock()
	leaseID := el.leaseID
	el.mutex.RUnlock()

	if leaseID == 0 {
		return false, nil
	}

	// A positive remaining TTL means the lease (and thus the lock) is still live.
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

	// Guard against double-acquire on the same instance: a second Acquire would
	// otherwise overwrite leaseID/cancel and orphan the previous lease and its
	// keepAlive goroutine. Reject re-acquire while the lock is still held.
	el.mutex.Lock()
	if el.leaseID != 0 {
		el.mutex.Unlock()
		currentCallback().OnLockAcquireFailed(el.key, ErrLockAlreadyHeld)
		return ErrLockAlreadyHeld
	}
	el.mutex.Unlock()

	lockKey := buildLockKey(el.key)

	// Create lease. Round the TTL up to whole seconds so the lease never expires
	// before the renewal logic (which uses the full sub-second duration) runs.
	lease, err := client.Grant(ctx, leaseTTLSeconds(el.expiration))
	if err != nil {
		currentCallback().OnLockAcquireFailed(el.key, err)
		return fmt.Errorf("failed to grant lease: %w", err)
	}

	// Atomically claim the key only if it does not yet exist (CreateRevision==0).
	// On contention the Else branch fetches the current holder for diagnostics.
	txn := client.Txn(ctx)
	txn.If(clientv3.Compare(clientv3.CreateRevision(lockKey), "=", 0)).
		Then(clientv3.OpPut(lockKey, "", clientv3.WithLease(lease.ID))).
		Else(clientv3.OpGet(lockKey))

	txnResp, err := txn.Commit()
	if err != nil {
		revokeLease(client, lease.ID, el.key)
		currentCallback().OnLockAcquireFailed(el.key, err)
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	if !txnResp.Succeeded {
		revokeLease(client, lease.ID, el.key)
		currentCallback().OnLockAcquireFailed(el.key, ErrLockAcquireConflict)
		return ErrLockAcquireConflict
	}

	now := time.Now()
	el.mutex.Lock()
	el.leaseID = lease.ID
	el.acquiredAt = now
	el.expiresAt = now.Add(el.expiration)
	renewalEnabled := el.renewalEnabled
	renewalThreshold := el.renewalThreshold
	el.mutex.Unlock()

	if renewalEnabled && renewalThreshold > 0 {
		el.mutex.Lock()
		el.ctx, el.cancel = context.WithCancel(context.Background())
		el.mutex.Unlock()
		go el.keepAlive()
	}

	currentCallback().OnLockAcquired(el.key, el.expiration)
	return nil
}

// AcquireWithRetry acquires the lock and retries according to strategy.
// A ±50% jitter is applied to each retry delay so that contending callers
// do not retry in lockstep (thundering herd).
func (el *EtcdLock) AcquireWithRetry(ctx context.Context, strategy RetryStrategy) error {
	retries := 0
	for {
		if strategy.MaxRetries > 0 && retries >= strategy.MaxRetries {
			return ErrMaxRetriesExceeded
		}
		if retries > 0 && strategy.RetryDelay > 0 {
			delay := timex.JitterAround(strategy.RetryDelay, 0.5)
			if !waitForRetryDelay(ctx, delay) {
				return ctx.Err()
			}
		}
		err := el.Acquire(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrLockAcquireConflict) {
			return err
		}
		// Only contention is retryable; with no retry budget, report the conflict.
		if strategy.MaxRetries == 0 {
			return ErrLockAcquireConflict
		}
		retries++
	}
}

// keepAlive keeps the lease alive. It recovers from panics so a failure in the
// etcd client can never crash the process, backs off exponentially between
// reconnection attempts, and signals terminal lock loss (via markLost) once the
// lease can no longer be kept alive.
func (el *EtcdLock) keepAlive() {
	defer func() {
		if r := recover(); r != nil {
			log.ErrorCtx(context.Background(), "keepAlive goroutine panicked", "key", el.key, "panic", r)
			el.markLost(fmt.Errorf("keepAlive panic: %v", r))
		}
	}()

	const (
		baseBackoff = 50 * time.Millisecond
		maxBackoff  = 5 * time.Second
		// maxKeepAliveFailures bounds reconnection attempts. Once the lease TTL
		// has almost certainly elapsed without a successful keep-alive, the lock
		// is considered permanently lost.
		maxKeepAliveFailures = 6
	)
	backoff := baseBackoff
	failures := 0

	for {
		el.mutex.RLock()
		ctx := el.ctx
		leaseID := el.leaseID
		el.mutex.RUnlock()
		if ctx == nil || leaseID == 0 {
			return
		}

		client, err := el.currentClient(ctx)
		if err != nil {
			log.ErrorCtx(ctx, "failed to resolve etcd client for keep alive", "error", err)
			if failures++; failures >= maxKeepAliveFailures {
				el.markLost(fmt.Errorf("keep alive: resolve client failed: %w", err))
				return
			}
			if !waitForRetryDelay(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		ch, kaErr := client.KeepAlive(ctx, leaseID)
		if kaErr != nil {
			log.ErrorCtx(ctx, "failed to start keep alive", "error", kaErr)
			if failures++; failures >= maxKeepAliveFailures {
				el.markLost(fmt.Errorf("keep alive: start failed: %w", kaErr))
				return
			}
			if !waitForRetryDelay(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		channelClosed := false
		for !channelClosed {
			select {
			case <-ctx.Done():
				return
			case ka, ok := <-ch:
				if !ok {
					log.WarnCtx(ctx, "keep alive channel closed", "key", el.key)
					channelClosed = true
					break
				}
				if ka != nil {
					// Successful keep-alive: lease renewed, reset failure state.
					failures = 0
					backoff = baseBackoff
					el.mutex.Lock()
					el.expiresAt = time.Now().Add(time.Duration(ka.TTL) * time.Second)
					el.mutex.Unlock()
				}
			}
		}

		// Channel closed: the lease may have been lost. Back off and retry,
		// giving up (and signalling terminal loss) after repeated failures.
		if failures++; failures >= maxKeepAliveFailures {
			el.markLost(ErrLockRenewalFailed)
			return
		}
		if !waitForRetryDelay(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// nextBackoff doubles the current backoff up to a ceiling.
func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// NewLock builds a reusable lock for key, validating the key and options and
// verifying the provider can resolve a client. It does not acquire the lock.
func NewLock(ctx context.Context, provider ClientProvider, key string, options LockOptions) (*EtcdLock, error) {
	if err := ValidateKey(key); err != nil {
		return nil, fmt.Errorf("invalid lock key: %w", err)
	}
	options = normalizeLockOptions(options)
	if err := options.Validate(); err != nil {
		return nil, fmt.Errorf("invalid lock options: %w", err)
	}
	if provider == nil {
		return nil, fmt.Errorf("etcd client provider not found")
	}
	if _, err := provider.Client(ctx); err != nil {
		return nil, fmt.Errorf("failed to resolve etcd client: %w", err)
	}

	lock := &EtcdLock{
		provider:         provider,
		key:              key,
		expiration:       options.Expiration,
		renewalThreshold: options.RenewalThreshold,
		renewalEnabled:   options.RenewalEnabled,
	}
	return lock, nil
}
