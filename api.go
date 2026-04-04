package etcdlock

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/go-lynx/lynx/log"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Global callback instance
var globalCallback LockCallback = NoOpCallback{}

// SetCallback sets the global callback
func SetCallback(callback LockCallback) {
	if callback == nil {
		callback = NoOpCallback{}
	}
	globalCallback = callback
}

// GetEtcdClient gets the current etcd client through the registered provider.
var GetEtcdClient = func() *clientv3.Client {
	provider := GetClientProvider()
	if provider == nil {
		return nil
	}
	client, err := provider.Client(context.Background())
	if err != nil {
		return nil
	}
	return client
}

// Lock acquires a distributed lock for the specified key and executes the callback function, automatically releasing the lock after execution.
func Lock(ctx context.Context, key string, expiration time.Duration, fn func() error) error {
	// Use DefaultLockOptions as base configuration, only overriding Expiration
	options := DefaultLockOptions
	options.Expiration = expiration
	return LockWithOptions(ctx, key, options, fn)
}

// LockWithOptions uses complete configuration options to acquire lock and execute callback function.
func LockWithOptions(ctx context.Context, key string, options LockOptions, fn func() error) error {
	// Validate callback function
	if fn == nil {
		return ErrLockFnRequired
	}

	// Create lock instance
	lock, err := NewLockFromClient(ctx, key, options)
	if err != nil {
		return err
	}

	// Try to acquire lock
	if options.RetryStrategy.MaxRetries > 0 {
		err = lock.AcquireWithRetry(ctx, options.RetryStrategy)
	} else {
		err = lock.Acquire(ctx)
	}

	if err != nil {
		return err
	}

	// If renewal is enabled, include in global management
	if options.RenewalEnabled {
		lock.EnableAutoRenew(options)
	}

	// Ensure final release - use Background to avoid caller ctx cancellation blocking release
	defer func() {
		to := options.OperationTimeout
		if to <= 0 {
			to = DefaultLockOptions.OperationTimeout
		}
		rctx, cancel := context.WithTimeout(context.Background(), to)
		defer cancel()
		if releaseErr := lock.Release(rctx); releaseErr != nil {
			log.ErrorCtx(ctx, "failed to release etcd lock", "error", releaseErr)
		}
	}()

	// Execute user function
	return fn()
}

// LockWithRetry acquires lock and executes function, supports retry by strategy.
func LockWithRetry(ctx context.Context, key string, expiration time.Duration, fn func() error, strategy RetryStrategy) error {
	// Use DefaultLockOptions as base configuration, override Expiration and RetryStrategy
	options := DefaultLockOptions
	options.Expiration = expiration
	options.RetryStrategy = strategy
	return LockWithOptions(ctx, key, options, fn)
}

// NewLockFromClient creates a reusable lock instance from the current etcd client provider.
func NewLockFromClient(ctx context.Context, key string, options LockOptions) (*EtcdLock, error) {
	provider, err := resolveClientProvider()
	if err != nil {
		return nil, err
	}

	return NewLock(ctx, provider, key, options)
}

// EnableAutoRenew registers the current lock to the global renewal manager.
// Cancels per-lock keepAlive if running (manager handles renewal instead).
func (el *EtcdLock) EnableAutoRenew(options LockOptions) {
	// Stop per-lock keepAlive - manager renewal takes over
	el.mutex.Lock()
	if el.cancel != nil {
		el.cancel()
		el.cancel = nil
		el.ctx = nil
	}
	el.mutex.Unlock()

	globalLockManager.mutex.Lock()
	if _, exists := globalLockManager.locks[el.key]; !exists {
		globalLockManager.locks[el.key] = el
		atomic.AddInt64(&globalLockManager.stats.ActiveLocks, 1)
		atomic.AddInt64(&globalLockManager.stats.TotalLocks, 1)
	}
	globalLockManager.mutex.Unlock()
	globalLockManager.startRenewalService(options)
}
