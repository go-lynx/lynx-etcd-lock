package etcdlock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-lynx/lynx/log"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var (
	globalCallbackMu sync.RWMutex
	globalCallback   LockCallback = NoOpCallback{}
)

// SetCallback installs the process-wide lock-event callback; nil resets to no-op.
func SetCallback(callback LockCallback) {
	if callback == nil {
		callback = NoOpCallback{}
	}
	globalCallbackMu.Lock()
	globalCallback = callback
	globalCallbackMu.Unlock()
}

func currentCallback() LockCallback {
	globalCallbackMu.RLock()
	callback := globalCallback
	globalCallbackMu.RUnlock()
	if callback == nil {
		return NoOpCallback{}
	}
	return callback
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

// Lock acquires the lock for key with the default options (only the expiration
// overridden), runs fn in the critical section, and releases the lock afterwards.
func Lock(ctx context.Context, key string, expiration time.Duration, fn func() error) error {
	options := DefaultLockOptions
	options.Expiration = expiration
	return LockWithOptions(ctx, key, options, fn)
}

// LockWithOptionsCtx acquires the lock with the given options, runs fn(lockCtx) while
// holding it, and always releases on return. lockCtx is derived from ctx and is
// additionally cancelled when the lock is permanently lost, giving fn a chance to abort
// its critical section via context cancellation. After cancelling lockCtx,
// LockWithOptionsCtx waits up to OperationTimeout for fn to return before moving on.
func LockWithOptionsCtx(ctx context.Context, key string, options LockOptions, fn func(context.Context) error) (retErr error) {
	if fn == nil {
		return ErrLockFnRequired
	}
	options = normalizeLockOptions(options)

	lock, err := NewLockFromClient(ctx, key, options)
	if err != nil {
		return err
	}

	if options.RetryStrategy.MaxRetries > 0 {
		err = lock.AcquireWithRetry(ctx, options.RetryStrategy)
	} else {
		err = lock.Acquire(ctx)
	}

	if err != nil {
		return err
	}

	if options.RenewalEnabled {
		lock.EnableAutoRenew(options)
	}

	// Release on a Background-derived context so a cancelled caller ctx cannot
	// block the release.
	defer func() {
		to := options.OperationTimeout
		if to <= 0 {
			to = DefaultLockOptions.OperationTimeout
		}
		rctx, cancel := context.WithTimeout(context.Background(), to)
		defer cancel()
		if releaseErr := lock.Release(rctx); releaseErr != nil {
			// When the lock was lost, retErr already wraps ErrLockLost and Release
			// returns ErrLockLost too; don't join it again.
			if errors.Is(retErr, ErrLockLost) && errors.Is(releaseErr, ErrLockLost) {
				return
			}
			log.ErrorCtx(ctx, "failed to release etcd lock", "error", releaseErr)
			retErr = errors.Join(retErr, releaseErr)
		}
	}()

	// lockCtx is cancelled when the lock is permanently lost, giving fn a chance
	// to detect the loss via ctx.Err() and abort its critical section gracefully.
	lockCtx, lockCancel := context.WithCancel(ctx)
	defer lockCancel()

	// Run fn in a goroutine so we can race it against lock loss.
	fnDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fnDone <- fmt.Errorf("lock callback panicked: %v", r)
			}
		}()
		fnDone <- fn(lockCtx)
	}()

	select {
	case err := <-fnDone:
		return err
	case <-lock.Done():
		lockCancel() // signal fn to abort via context
		lostErr := lock.LostErr()
		if lostErr == nil {
			lostErr = ErrLockLost
		}
		log.ErrorCtx(ctx, "etcd lock lost during critical section", "key", key, "error", lostErr)
		// Wait for fn to observe the cancellation and exit (bounded to prevent goroutine leak).
		to := options.OperationTimeout
		if to <= 0 {
			to = DefaultLockOptions.OperationTimeout
		}
		timer := time.NewTimer(to)
		defer timer.Stop()
		select {
		case <-fnDone:
		case <-timer.C:
			log.ErrorCtx(ctx, "fn did not stop within timeout after lock loss", "key", key)
		}
		return errors.Join(ErrLockLost, lostErr)
	}
}

// LockWithOptions acquires the lock with the given options, runs fn while holding
// it, and always releases on return. If the lock is lost mid-section fn is aborted
// and an ErrLockLost-wrapped error is returned.
func LockWithOptions(ctx context.Context, key string, options LockOptions, fn func() error) error {
	if fn == nil {
		return ErrLockFnRequired
	}
	return LockWithOptionsCtx(ctx, key, options, func(_ context.Context) error { return fn() })
}

// LockWithRetry is Lock with a caller-supplied retry strategy for contention.
func LockWithRetry(ctx context.Context, key string, expiration time.Duration, fn func() error, strategy RetryStrategy) error {
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
	options = normalizeLockOptions(options)
	// Stop per-lock keepAlive - manager renewal takes over
	el.mutex.Lock()
	if el.cancel != nil {
		el.cancel()
		el.cancel = nil
		el.ctx = nil
	}
	el.mutex.Unlock()

	globalLockManager.addManagedLock(el)
	globalLockManager.startRenewalService(options)
}
