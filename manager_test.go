package etcdlock

import (
	"context"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type recordingCallback struct {
	mu       sync.Mutex
	acquire  int
	release  int
	renew    int
	failures int
}

func (c *recordingCallback) OnLockAcquired(string, time.Duration) {
	c.mu.Lock()
	c.acquire++
	c.mu.Unlock()
}

func (c *recordingCallback) OnLockReleased(string, time.Duration) {
	c.mu.Lock()
	c.release++
	c.mu.Unlock()
}

func (c *recordingCallback) OnLockRenewed(string, time.Duration) {
	c.mu.Lock()
	c.renew++
	c.mu.Unlock()
}

func (c *recordingCallback) OnLockRenewalFailed(string, error) {
	c.mu.Lock()
	c.failures++
	c.mu.Unlock()
}

func (c *recordingCallback) OnLockAcquireFailed(string, error) {
	c.mu.Lock()
	c.failures++
	c.mu.Unlock()
}

func TestNormalizeLockOptionsFillsPartialConfig(t *testing.T) {
	options := normalizeLockOptions(LockOptions{Expiration: 5 * time.Second})

	if options.RetryStrategy != DefaultRetryStrategy {
		t.Fatalf("retry strategy = %#v, want %#v", options.RetryStrategy, DefaultRetryStrategy)
	}
	if options.RenewalConfig != DefaultRenewalConfig {
		t.Fatalf("renewal config = %#v, want %#v", options.RenewalConfig, DefaultRenewalConfig)
	}
	if options.WorkerPoolSize != DefaultLockOptions.WorkerPoolSize {
		t.Fatalf("worker pool size = %d, want %d", options.WorkerPoolSize, DefaultLockOptions.WorkerPoolSize)
	}
}

func TestLockOptionsRejectSubSecondExpiration(t *testing.T) {
	options := DefaultLockOptions
	options.Expiration = 500 * time.Millisecond

	if err := options.Validate(); err == nil {
		t.Fatal("expected sub-second expiration to be rejected")
	}
}

func TestNewLockHonorsRenewalEnabled(t *testing.T) {
	options := DefaultLockOptions
	options.RenewalEnabled = false

	lock, err := NewLock(context.Background(), fakeClientProvider{client: &clientv3.Client{}}, "renew-disabled", options)
	if err != nil {
		t.Fatalf("new lock failed: %v", err)
	}
	if lock.renewalEnabled {
		t.Fatal("expected renewalEnabled to be false")
	}
}

func TestLockManagerAddRemoveIsIdentitySafe(t *testing.T) {
	manager := &lockManager{locks: make(map[string]*EtcdLock)}
	first := &EtcdLock{key: "same-key"}
	second := &EtcdLock{key: "same-key"}

	manager.addManagedLock(first)
	manager.addManagedLock(second)
	manager.removeLock(first)

	if got := manager.locks["same-key"]; got != second {
		t.Fatalf("expected replacement lock to remain managed, got %#v", got)
	}
	if active := manager.stats.ActiveLocks; active != 1 {
		t.Fatalf("active locks = %d, want 1", active)
	}

	manager.removeLock(second)
	manager.removeLock(second)
	if active := manager.stats.ActiveLocks; active != 0 {
		t.Fatalf("active locks = %d, want 0", active)
	}
}

func TestSetCallbackConcurrent(t *testing.T) {
	previous := currentCallback()
	t.Cleanup(func() { SetCallback(previous) })

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			SetCallback(&recordingCallback{})
			currentCallback().OnLockAcquired("key", time.Second)
		}()
	}
	wg.Wait()

	SetCallback(nil)
	if _, ok := currentCallback().(NoOpCallback); !ok {
		t.Fatal("expected nil callback to reset to NoOpCallback")
	}
}
