package etcdlock

import (
	"context"
	"errors"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// ── AcquireWithRetry retry-loop tests ─────────────────────────────────────────
//
// These tests drive the retry loop through a synthetic acquireFn rather than a
// real etcd connection, which would be needed to produce ErrLockAcquireConflict
// from the transaction path.  The helpers below stub out the acquisition step
// so the retry / timeout logic can be exercised in isolation.

// simulateAcquireWithRetry is a copy of the pure retry-loop logic from
// EtcdLock.AcquireWithRetry, extracted here so we can inject any acquireFn.
func simulateAcquireWithRetry(ctx context.Context, acquireFn func() error, strategy RetryStrategy) error {
	retries := 0
	for {
		if strategy.MaxRetries > 0 && retries >= strategy.MaxRetries {
			return ErrMaxRetriesExceeded
		}
		if retries > 0 && strategy.RetryDelay > 0 {
			if !waitForRetryDelay(ctx, strategy.RetryDelay) {
				return ctx.Err()
			}
		}
		err := acquireFn()
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrLockAcquireConflict) {
			return err
		}
		if strategy.MaxRetries == 0 {
			return ErrLockAcquireConflict
		}
		retries++
	}
}

// TestAcquireWithRetry_ContextCancellation verifies that the retry loop
// respects context cancellation and does not block beyond the deadline.
func TestAcquireWithRetry_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	strategy := RetryStrategy{MaxRetries: 1000, RetryDelay: 5 * time.Millisecond}
	start := time.Now()
	err := simulateAcquireWithRetry(ctx, func() error {
		return ErrLockAcquireConflict
	}, strategy)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("retry loop exceeded context deadline: elapsed=%v", elapsed)
	}
}

// TestAcquireWithRetry_MaxRetriesExhausted verifies ErrMaxRetriesExceeded is
// returned once all retry attempts are consumed.
func TestAcquireWithRetry_MaxRetriesExhausted(t *testing.T) {
	calls := 0
	strategy := RetryStrategy{MaxRetries: 3, RetryDelay: time.Millisecond}
	err := simulateAcquireWithRetry(context.Background(), func() error {
		calls++
		return ErrLockAcquireConflict
	}, strategy)
	if !errors.Is(err, ErrMaxRetriesExceeded) {
		t.Fatalf("expected ErrMaxRetriesExceeded, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected exactly 3 acquire attempts, got %d", calls)
	}
}

// TestAcquireWithRetry_ZeroMaxRetries_ImmediateConflict verifies that a zero
// MaxRetries strategy never retries; it returns ErrLockAcquireConflict on the
// first (and only) attempt.
func TestAcquireWithRetry_ZeroMaxRetries_ImmediateConflict(t *testing.T) {
	calls := 0
	strategy := RetryStrategy{MaxRetries: 0}
	err := simulateAcquireWithRetry(context.Background(), func() error {
		calls++
		return ErrLockAcquireConflict
	}, strategy)
	if !errors.Is(err, ErrLockAcquireConflict) {
		t.Fatalf("expected ErrLockAcquireConflict, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 acquire attempt, got %d", calls)
	}
}

// TestAcquireWithRetry_NonConflictErrorNoRetry verifies that non-conflict errors
// are returned immediately without retrying.
func TestAcquireWithRetry_NonConflictErrorNoRetry(t *testing.T) {
	sentinel := errors.New("network error")
	calls := 0
	strategy := RetryStrategy{MaxRetries: 10, RetryDelay: time.Millisecond}
	err := simulateAcquireWithRetry(context.Background(), func() error {
		calls++
		return sentinel
	}, strategy)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 attempt for non-conflict error, got %d", calls)
	}
}

// ── Release / Renew guard tests ───────────────────────────────────────────────

// TestRelease_NotHeld verifies that Release returns ErrLockNotHeld when the
// lock was never acquired (leaseID == 0), i.e. before Grant+Txn have run.
// No real etcd RPC is made because the leaseID guard fires first.
func TestRelease_NotHeld(t *testing.T) {
	lock := &EtcdLock{
		provider:   fakeClientProvider{client: &clientv3.Client{}},
		key:        "not-held",
		expiration: 30 * time.Second,
		leaseID:    0, // never acquired
	}
	err := lock.Release(context.Background())
	if !errors.Is(err, ErrLockNotHeld) {
		t.Fatalf("expected ErrLockNotHeld, got %v", err)
	}
}

// TestRenew_NotHeld verifies that Renew returns ErrLockNotHeld when the
// lock was never acquired. No RPC is made.
func TestRenew_NotHeld(t *testing.T) {
	lock := &EtcdLock{
		provider:   fakeClientProvider{client: &clientv3.Client{}},
		key:        "not-held-renew",
		expiration: 30 * time.Second,
		leaseID:    0, // never acquired
	}
	err := lock.Renew(context.Background(), 0)
	if !errors.Is(err, ErrLockNotHeld) {
		t.Fatalf("expected ErrLockNotHeld, got %v", err)
	}
}

// ── IsExpired / GetRemainingTime tests ────────────────────────────────────────

// TestIsExpired_ReturnsTrueForPastExpiry verifies IsExpired for a lock whose
// expiresAt is in the past.
func TestIsExpired_ReturnsTrueForPastExpiry(t *testing.T) {
	lock := &EtcdLock{
		key:       "expired",
		expiresAt: time.Now().Add(-time.Second),
	}
	if !lock.IsExpired() {
		t.Fatal("expected lock to be expired")
	}
}

// TestIsExpired_ReturnsFalseForFutureExpiry verifies IsExpired for a lock with
// a future expiry.
func TestIsExpired_ReturnsFalseForFutureExpiry(t *testing.T) {
	lock := &EtcdLock{
		key:       "fresh",
		expiresAt: time.Now().Add(time.Minute),
	}
	if lock.IsExpired() {
		t.Fatal("expected lock to not be expired")
	}
}

// ── ValidateKey tests ─────────────────────────────────────────────────────────

func TestValidateKey_EmptyKeyRejected(t *testing.T) {
	if err := ValidateKey(""); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestValidateKey_TooLongRejected(t *testing.T) {
	key := make([]byte, 256)
	for i := range key {
		key[i] = 'a'
	}
	if err := ValidateKey(string(key)); err == nil {
		t.Fatal("expected error for key > 255 bytes")
	}
}

func TestValidateKey_ValidKey(t *testing.T) {
	if err := ValidateKey("order-service/lock-1"); err != nil {
		t.Fatalf("expected valid key to pass, got: %v", err)
	}
}

// ── LockOptions.Validate tests ────────────────────────────────────────────────

func TestLockOptions_NegativeRenewalThresholdRejected(t *testing.T) {
	opts := DefaultLockOptions
	opts.RenewalThreshold = -0.1
	if err := opts.Validate(); err == nil {
		t.Fatal("expected error for negative renewal threshold")
	}
}

func TestLockOptions_ThresholdAboveOneRejected(t *testing.T) {
	opts := DefaultLockOptions
	opts.RenewalThreshold = 1.1
	if err := opts.Validate(); err == nil {
		t.Fatal("expected error for renewal threshold > 1")
	}
}
