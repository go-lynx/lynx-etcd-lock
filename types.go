package etcdlock

import (
	"context"
	"fmt"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// LockOptions lock configuration options
type LockOptions struct {
	Expiration       time.Duration // Lock expiration time
	RetryStrategy    RetryStrategy // Retry strategy
	RenewalEnabled   bool          // Whether to enable auto renewal
	RenewalThreshold float64       // Renewal threshold (proportion relative to expiration time, default 1/3)
	WorkerPoolSize   int           // Renewal worker pool size, default 50
	RenewalConfig    RenewalConfig // Renewal configuration
	// OperationTimeout timeout control for single operation (acquire/release). 0 means no separate timeout.
	OperationTimeout time.Duration
}

// Validate validates configuration options
func (lo *LockOptions) Validate() error {
	if lo.Expiration <= 0 {
		return fmt.Errorf("expiration must be positive, got %v", lo.Expiration)
	}

	if lo.RenewalThreshold < 0 || lo.RenewalThreshold > 1 {
		return fmt.Errorf("renewal threshold must be between 0 and 1, got %f", lo.RenewalThreshold)
	}

	if lo.WorkerPoolSize < 0 {
		return fmt.Errorf("worker pool size must be non-negative, got %d", lo.WorkerPoolSize)
	}

	return lo.RetryStrategy.Validate()
}

// ValidateKey validates the validity of lock key name
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("lock key cannot be empty")
	}

	if len(key) > 255 {
		return fmt.Errorf("lock key too long, max length is 255, got %d", len(key))
	}

	return nil
}

// Validate validates retry strategy
func (rs *RetryStrategy) Validate() error {
	if rs.MaxRetries < 0 {
		return fmt.Errorf("max retries must be non-negative, got %d", rs.MaxRetries)
	}

	if rs.RetryDelay < 0 {
		return fmt.Errorf("retry delay must be non-negative, got %v", rs.RetryDelay)
	}

	return nil
}

// RetryStrategy defines lock retry strategy
type RetryStrategy struct {
	MaxRetries int           // Maximum retry attempts
	RetryDelay time.Duration // Retry interval
}

// RenewalConfig renewal configuration
type RenewalConfig struct {
	MaxRetries    int           // Maximum renewal retry attempts
	BaseDelay     time.Duration // Base retry delay
	MaxDelay      time.Duration // Maximum retry delay
	CheckInterval time.Duration // Renewal check interval
	// OperationTimeout single renewal operation timeout. 0 means no separate timeout.
	OperationTimeout time.Duration
}

// LockCallback lock operation callback interface
type LockCallback interface {
	OnLockAcquired(key string, duration time.Duration)
	OnLockReleased(key string, duration time.Duration)
	OnLockRenewed(key string, duration time.Duration)
	OnLockRenewalFailed(key string, error error)
	OnLockAcquireFailed(key string, error error)
}

// NoOpCallback empty implementation callback
type NoOpCallback struct{}

func (NoOpCallback) OnLockAcquired(key string, duration time.Duration) {}
func (NoOpCallback) OnLockReleased(key string, duration time.Duration) {}
func (NoOpCallback) OnLockRenewed(key string, duration time.Duration)  {}
func (NoOpCallback) OnLockRenewalFailed(key string, error error)       {}
func (NoOpCallback) OnLockAcquireFailed(key string, error error)       {}

// EtcdLock implements etcd-based distributed lock
type EtcdLock struct {
	client           *clientv3.Client
	key              string
	leaseID          clientv3.LeaseID
	expiration       time.Duration
	expiresAt        time.Time
	mutex            sync.Mutex
	renewalThreshold float64
	acquiredAt       time.Time
	ctx              context.Context
	cancel           context.CancelFunc
}

// Default configurations
var (
	DefaultRetryStrategy = RetryStrategy{
		MaxRetries: 3,
		RetryDelay: 100 * time.Millisecond,
	}

	DefaultRenewalConfig = RenewalConfig{
		MaxRetries:       4,
		BaseDelay:        100 * time.Millisecond,
		MaxDelay:         800 * time.Millisecond,
		CheckInterval:    300 * time.Millisecond,
		OperationTimeout: 600 * time.Millisecond,
	}

	DefaultLockOptions LockOptions
)

// init initializes default configurations
func init() {
	DefaultLockOptions = LockOptions{
		Expiration:       30 * time.Second,
		RetryStrategy:    DefaultRetryStrategy,
		RenewalEnabled:   true,
		RenewalThreshold: 0.3,
		WorkerPoolSize:   50,
		RenewalConfig:    DefaultRenewalConfig,
		OperationTimeout: 600 * time.Millisecond,
	}
}

// buildLockKey generates actual etcd key based on business key
func buildLockKey(base string) string {
	return fmt.Sprintf("lynx/lock/%s", base)
}
