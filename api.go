package etcdlock

import (
	"context"
	"fmt"
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

// GetEtcdClient gets the etcd client from the plugin
var GetEtcdClient = func() *clientv3.Client {
	// This will be set by the plugin during initialization
	return nil
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

	// Get etcd client
	client := GetEtcdClient()
	if client == nil {
		return fmt.Errorf("etcd client not found")
	}

	// Create lock instance
	lock, err := NewLock(ctx, client, key, options)
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

	// Ensure final release
	defer func() {
		rctx := ctx
		var cancel context.CancelFunc
		to := options.OperationTimeout
		if to <= 0 {
			to = DefaultLockOptions.OperationTimeout
		}
		if to > 0 {
			rctx, cancel = context.WithTimeout(ctx, to)
		}
		start := time.Now()
		if releaseErr := lock.Release(rctx); releaseErr != nil {
			log.ErrorCtx(ctx, "failed to release etcd lock", "error", releaseErr)
		}
		if cancel != nil {
			cancel()
		}
		observeOperationLatency("unlock", time.Since(start))
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

// NewLockFromClient creates a reusable lock instance from an etcd client
func NewLockFromClient(ctx context.Context, key string, options LockOptions) (*EtcdLock, error) {
	// Get etcd client
	client := GetEtcdClient()
	if client == nil {
		return nil, fmt.Errorf("etcd client not found")
	}

	return NewLock(ctx, client, key, options)
}

// EnableAutoRenew registers the current lock to the global renewal manager
func (el *EtcdLock) EnableAutoRenew(options LockOptions) {
	globalLockManager.mutex.Lock()
	if _, exists := globalLockManager.locks[el.key]; !exists {
		globalLockManager.locks[el.key] = el
	}
	globalLockManager.mutex.Unlock()
	globalLockManager.startRenewalService(options)
}

// observeOperationLatency observes operation latency (placeholder for metrics)
func observeOperationLatency(operation string, duration time.Duration) {
	// This can be extended to record metrics
}
