package etcdlock

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-lynx/lynx/log"
)

// Global lock manager
var globalLockManager = &lockManager{
	locks: make(map[string]*EtcdLock),
}

// lockManager manages all distributed lock instances
type lockManager struct {
	mutex sync.RWMutex
	locks map[string]*EtcdLock
	// Renewal service
	renewCtx    context.Context
	renewCancel context.CancelFunc
	running     bool
	// Worker pool
	workerPool chan struct{}
	// Statistics
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

// rng provides package-local randomness
var (
	rng   = rand.New(rand.NewSource(time.Now().UnixNano()))
	rngMu sync.Mutex
)

// randFloat64 returns a random float64 in [0.0, 1.0)
func randFloat64() float64 {
	rngMu.Lock()
	v := rng.Float64()
	rngMu.Unlock()
	return v
}

// startRenewalService starts the renewal service
func (lm *lockManager) startRenewalService(options LockOptions) {
	lm.mutex.Lock()
	if lm.running {
		lm.mutex.Unlock()
		return
	}
	lm.renewCtx, lm.renewCancel = context.WithCancel(context.Background())
	lm.running = true
	// Initialize worker pool
	workerPoolSize := options.WorkerPoolSize
	if workerPoolSize <= 0 {
		workerPoolSize = DefaultLockOptions.WorkerPoolSize
	}
	lm.workerPool = make(chan struct{}, workerPoolSize)
	lm.mutex.Unlock()

	go func() {
		checkInterval := options.RenewalConfig.CheckInterval
		if checkInterval <= 0 {
			checkInterval = DefaultRenewalConfig.CheckInterval
		}
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				lm.processRenewals(options)
			case <-lm.renewCtx.Done():
				return
			}
		}
	}()
}

// removeLock removes a lock from the manager and updates stats
func (lm *lockManager) removeLock(key string) {
	lm.mutex.Lock()
	if _, exists := lm.locks[key]; exists {
		delete(lm.locks, key)
		atomic.AddInt64(&lm.stats.ActiveLocks, -1)
	}
	lm.mutex.Unlock()
}

// stopRenewalService stops the renewal service
func (lm *lockManager) stopRenewalService() {
	lm.mutex.Lock()
	defer lm.mutex.Unlock()

	if !lm.running {
		return
	}

	lm.renewCancel()
	lm.running = false
}

// processRenewals processes lock renewals
func (lm *lockManager) processRenewals(options LockOptions) {
	lm.mutex.RLock()

	locksToRenew := make([]*EtcdLock, 0, len(lm.locks))

	for _, lock := range lm.locks {
		lock.mutex.Lock()
		expiresAtSnap := lock.expiresAt
		expirationSnap := lock.expiration
		thresholdSnap := lock.renewalThreshold
		lock.mutex.Unlock()

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
				lm.renewLockWithRetry(l, options)
			}(lock)
		default:
			atomic.AddInt64(&lm.stats.SkippedRenewals, 1)
		}
	}
}

// renewLockWithRetry lock renewal with retry
func (lm *lockManager) renewLockWithRetry(lock *EtcdLock, options LockOptions) {
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
			delay := config.BaseDelay * time.Duration(1<<i)
			if delay > 0 {
				jitter := time.Duration(float64(delay) * (0.5 + randFloat64()))
				if jitter > 0 {
					delay = jitter
				}
			}
			if delay > config.MaxDelay {
				delay = config.MaxDelay
			}
			time.Sleep(delay)
		}
	}

	lm.mutex.Lock()
	if _, exists := lm.locks[lock.key]; exists {
		delete(lm.locks, lock.key)
		atomic.AddInt64(&lm.stats.ActiveLocks, -1)
	}
	lm.mutex.Unlock()

	log.ErrorCtx(context.Background(), "lock renewal failed after retries",
		"key", lock.key, "retries", maxRetries)
}

// renewLock renew a single lock
func (lm *lockManager) renewLock(ctx context.Context, lock *EtcdLock) error {
	lock.mutex.Lock()
	expiresAtSnap := lock.expiresAt
	expirationSnap := lock.expiration
	thresholdSnap := lock.renewalThreshold
	leaseID := lock.leaseID
	lock.mutex.Unlock()

	if time.Until(expiresAtSnap) > time.Duration(float64(expirationSnap)*thresholdSnap) {
		return nil
	}

	if leaseID == 0 {
		return ErrLockNotHeld
	}

	start := time.Now()
	_, err := lock.client.KeepAliveOnce(ctx, leaseID)
	latency := time.Since(start)
	atomic.AddInt64(&lm.stats.RenewLatencyNs, latency.Nanoseconds())
	atomic.AddInt64(&lm.stats.RenewLatencyCount, 1)

	if err != nil {
		return fmt.Errorf("keep alive failed: %w", err)
	}

	lock.mutex.Lock()
	lock.expiresAt = time.Now().Add(lock.expiration)
	lock.mutex.Unlock()

	return nil
}

// GetStats gets lock manager statistics
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
// Waits for all managed locks to be released (or timeout).
func Shutdown(ctx context.Context) error {
	globalLockManager.stopRenewalService()

	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			globalLockManager.mutex.RLock()
			n := len(globalLockManager.locks)
			globalLockManager.mutex.RUnlock()
			return fmt.Errorf("shutdown timeout, %d locks still active", n)
		case <-ticker.C:
			globalLockManager.mutex.RLock()
			n := len(globalLockManager.locks)
			globalLockManager.mutex.RUnlock()
			if n == 0 {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
