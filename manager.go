package etcdlock

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-lynx/lynx/log"
	"github.com/go-lynx/lynx/pkg/timex"
)

var globalLockManager = &lockManager{
	locks: make(map[string]*EtcdLock),
}

// lockManager renews every auto-renew lock from a single background ticker,
// dispatching renewals through a bounded worker pool.
type lockManager struct {
	mutex sync.RWMutex
	locks map[string]*EtcdLock

	renewCtx    context.Context
	renewCancel context.CancelFunc
	running     bool

	workerPool chan struct{}

	stats struct {
		TotalLocks        int64
		ActiveLocks       int64
		RenewalCount      int64
		RenewalErrors     int64
		SkippedRenewals   int64
		RenewLatencyNs    int64
		RenewLatencyCount int64
		WorkerPoolCap     int
	}
}

// startRenewalService starts the renewal ticker once; repeat calls are no-ops
// while it is running.
func (lm *lockManager) startRenewalService(options LockOptions) {
	options = normalizeLockOptions(options)
	lm.mutex.Lock()
	if lm.running {
		lm.mutex.Unlock()
		return
	}
	lm.renewCtx, lm.renewCancel = context.WithCancel(context.Background())
	lm.running = true
	workerPoolSize := options.WorkerPoolSize
	if workerPoolSize <= 0 {
		workerPoolSize = DefaultLockOptions.WorkerPoolSize
	}
	lm.workerPool = make(chan struct{}, workerPoolSize)
	lm.mutex.Unlock()

	checkInterval := options.RenewalConfig.CheckInterval
	if checkInterval <= 0 {
		checkInterval = DefaultRenewalConfig.CheckInterval
	}

	go func() {
		// Mark the service as stopped when this goroutine exits (normal shutdown or after unrecoverable error).
		defer func() {
			lm.mutex.Lock()
			lm.running = false
			lm.mutex.Unlock()
		}()

		// Capture the context once so restarts after a panic still respect the same cancel signal.
		lm.mutex.RLock()
		renewCtx := lm.renewCtx
		lm.mutex.RUnlock()

		for {
			// Run one ticker epoch inside a nested closure so a panic can be recovered
			// without killing the goroutine; the outer loop then restarts.
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.ErrorCtx(context.Background(), "panic in renewal service goroutine", "recover", r)
					}
				}()
				ticker := time.NewTicker(checkInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						lm.processRenewals(options)
					case <-renewCtx.Done():
						return
					}
				}
			}()

			// Inner closure returned: either context cancelled (clean shutdown) or panic recovered.
			select {
			case <-renewCtx.Done():
				return
			default:
				log.ErrorCtx(context.Background(), "renewal service restarting after panic recovery")
				waitForRetryDelay(renewCtx, 200*time.Millisecond)
			}
		}
	}()
}

func (lm *lockManager) addManagedLock(lock *EtcdLock) {
	if lock == nil {
		return
	}
	lm.mutex.Lock()
	_, exists := lm.locks[lock.key]
	lm.locks[lock.key] = lock
	if !exists {
		atomic.AddInt64(&lm.stats.ActiveLocks, 1)
		atomic.AddInt64(&lm.stats.TotalLocks, 1)
	}
	lm.mutex.Unlock()
}

func (lm *lockManager) decrementActiveLocks() {
	for {
		current := atomic.LoadInt64(&lm.stats.ActiveLocks)
		if current <= 0 {
			return
		}
		if atomic.CompareAndSwapInt64(&lm.stats.ActiveLocks, current, current-1) {
			return
		}
	}
}

// removeLock removes a lock from the manager and updates stats if the identity matches.
func (lm *lockManager) removeLock(lock *EtcdLock) {
	if lock == nil {
		return
	}
	lm.mutex.Lock()
	if existing, exists := lm.locks[lock.key]; exists && existing == lock {
		delete(lm.locks, lock.key)
		lm.decrementActiveLocks()
	}
	lm.mutex.Unlock()
}

func (lm *lockManager) stopRenewalService() {
	lm.mutex.Lock()
	defer lm.mutex.Unlock()

	if !lm.running {
		return
	}

	lm.renewCancel()
	lm.running = false
}

// processRenewals renews every lock whose remaining TTL has dropped below its
// renewal threshold, fanning the work out across the worker pool.
func (lm *lockManager) processRenewals(options LockOptions) {
	options = normalizeLockOptions(options)
	lm.mutex.RLock()

	locksToRenew := make([]*EtcdLock, 0, len(lm.locks))

	for _, lock := range lm.locks {
		lock.mutex.RLock()
		expiresAtSnap := lock.expiresAt
		expirationSnap := lock.expiration
		thresholdSnap := lock.renewalThreshold
		lock.mutex.RUnlock()

		thresholdDur := time.Duration(float64(expirationSnap) * thresholdSnap)
		if time.Until(expiresAtSnap) <= thresholdDur {
			locksToRenew = append(locksToRenew, lock)
		}
	}
	lm.mutex.RUnlock()

	for _, lock := range locksToRenew {
		select {
		case <-lm.renewCtx.Done():
			return
		case lm.workerPool <- struct{}{}:
			go func(l *EtcdLock) {
				defer func() { <-lm.workerPool }()
				// Recover so a panic in renewal (e.g. inside the etcd client)
				// cannot crash the whole process. A panicking renewal means we
				// can no longer guarantee the lock, so treat it as a loss.
				defer func() {
					if r := recover(); r != nil {
						log.ErrorCtx(context.Background(), "lock renewal worker panicked",
							"key", l.key, "panic", r)
						l.markLost(fmt.Errorf("renewal worker panic: %v", r))
					}
				}()
				lm.renewLockWithRetry(l, options)
			}(lock)
		default:
			atomic.AddInt64(&lm.stats.SkippedRenewals, 1)
		}
	}
}

// renewLockWithRetry renews a lock, retrying with exponential backoff; on
// terminal failure it marks the lock permanently lost.
func (lm *lockManager) renewLockWithRetry(lock *EtcdLock, options LockOptions) {
	options = normalizeLockOptions(options)
	config := options.RenewalConfig
	maxRetries := config.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultRenewalConfig.MaxRetries
	}

	for i := 0; i < maxRetries; i++ {
		ctx := lm.renewCtx
		var cancel context.CancelFunc
		if to := config.OperationTimeout; to > 0 {
			ctx, cancel = context.WithTimeout(ctx, to)
		}

		err := lm.renewLock(ctx, lock)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			atomic.AddInt64(&lm.stats.RenewalCount, 1)
			return
		}

		atomic.AddInt64(&lm.stats.RenewalErrors, 1)

		if i < maxRetries-1 {
			delay := timex.ExponentialBackoff(config.BaseDelay, config.MaxDelay, i, 0.5)
			if !waitForRetryDelay(lm.renewCtx, delay) {
				return
			}
		}
	}

	// Terminal renewal failure: the lease has (or will) expire and etcd will
	// auto-delete the key, so the lock is permanently lost. markLost cancels the
	// holder, forces a not-held state, drops it from the manager and notifies the
	// caller via Done()/OnLockLost so two nodes cannot run the critical section.
	lock.markLost(ErrLockRenewalFailed)

	log.ErrorCtx(context.Background(), "lock renewal failed after retries",
		"key", lock.key, "retries", maxRetries)
}

// renewLock performs one KeepAliveOnce for a single lock, skipping the call if
// the lock is still comfortably above its renewal threshold.
func (lm *lockManager) renewLock(ctx context.Context, lock *EtcdLock) error {
	client, err := lock.currentClient(ctx)
	if err != nil {
		return err
	}

	lock.mutex.RLock()
	expiresAtSnap := lock.expiresAt
	expirationSnap := lock.expiration
	thresholdSnap := lock.renewalThreshold
	leaseID := lock.leaseID
	lock.mutex.RUnlock()

	if time.Until(expiresAtSnap) > time.Duration(float64(expirationSnap)*thresholdSnap) {
		return nil
	}

	if leaseID == 0 {
		return ErrLockNotHeld
	}

	start := time.Now()
	resp, err := client.KeepAliveOnce(ctx, leaseID)
	latency := time.Since(start)
	atomic.AddInt64(&lm.stats.RenewLatencyNs, latency.Nanoseconds())
	atomic.AddInt64(&lm.stats.RenewLatencyCount, 1)
	observeOperationLatency("renew", operationStatus(err), latency)

	if err != nil {
		return fmt.Errorf("keep alive failed: %w", err)
	}

	lock.mutex.Lock()
	if resp != nil && resp.TTL > 0 {
		lock.expiresAt = time.Now().Add(time.Duration(resp.TTL) * time.Second)
	} else {
		lock.expiresAt = time.Now().Add(lock.expiration)
	}
	lock.mutex.Unlock()

	return nil
}

func waitForRetryDelay(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// GetStats returns a snapshot of the lock manager's counters and worker-queue depth.
func GetStats() map[string]int64 {
	m := map[string]int64{
		"total_locks":         atomic.LoadInt64(&globalLockManager.stats.TotalLocks),
		"active_locks":        atomic.LoadInt64(&globalLockManager.stats.ActiveLocks),
		"renewal_count":       atomic.LoadInt64(&globalLockManager.stats.RenewalCount),
		"renewal_errors":      atomic.LoadInt64(&globalLockManager.stats.RenewalErrors),
		"skipped_renewals":    atomic.LoadInt64(&globalLockManager.stats.SkippedRenewals),
		"renew_latency_ns":    atomic.LoadInt64(&globalLockManager.stats.RenewLatencyNs),
		"renew_latency_count": atomic.LoadInt64(&globalLockManager.stats.RenewLatencyCount),
	}
	if globalLockManager.workerPool != nil {
		m["worker_queue_len"] = int64(len(globalLockManager.workerPool))
		m["worker_queue_cap"] = int64(cap(globalLockManager.workerPool))
	}
	return m
}

// Shutdown gracefully shuts down the lock manager.
// It stops the renewal service and polls until all active locks are released or the
// context is cancelled. Callers control the deadline via ctx — e.g.:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//	_ = etcdlock.Shutdown(ctx)
func Shutdown(ctx context.Context) error {
	globalLockManager.stopRenewalService()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			globalLockManager.mutex.RLock()
			n := len(globalLockManager.locks)
			globalLockManager.mutex.RUnlock()
			if n == 0 {
				return nil
			}
		case <-ctx.Done():
			globalLockManager.mutex.RLock()
			n := len(globalLockManager.locks)
			globalLockManager.mutex.RUnlock()
			if n == 0 {
				return nil
			}
			return fmt.Errorf("shutdown cancelled with %d locks still active: %w", n, ctx.Err())
		}
	}
}
