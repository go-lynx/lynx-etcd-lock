# lynx-etcd-lock

Strongly consistent distributed lock plugin based on etcd for implementing distributed locks using etcd in the Lynx framework.

## Features

- ✅ Strongly consistent distributed lock based on etcd
- ✅ Automatic renewal support
- ✅ Retry strategy
- ✅ Lock timeout control
- ✅ Health check
- ✅ Built-in Prometheus metrics registration
- ✅ Graceful shutdown

## Configuration Example

This plugin depends on the `lynx-etcd` plugin, so you need to load the etcd configuration center plugin first.

```yaml
lynx:
  etcd:
    endpoints:
      - "127.0.0.1:2379"
    timeout: 10s
    namespace: "lynx/config"
  etcd-lock:
    # Lock configuration is set through code options
```

## Usage Example

```go
import (
    "context"
    "time"
    "github.com/go-lynx/lynx/plugins/etcd-lock"
)

// Simple usage
err := etcdlock.Lock(ctx, "my-lock-key", 30*time.Second, func() error {
    // Execute business logic that requires locking
    return nil
})

// Using options
options := etcdlock.LockOptions{
    Expiration:     30 * time.Second,
    RenewalEnabled: true,
    RetryStrategy: etcdlock.RetryStrategy{
        MaxRetries: 3,
        RetryDelay: 100 * time.Millisecond,
    },
}
err := etcdlock.LockWithOptions(ctx, "my-lock-key", options, func() error {
    // Execute business logic that requires locking
    return nil
})
```

## API

### Lock
Acquire a lock and execute a callback function, automatically release the lock after execution completes.

```go
func Lock(ctx context.Context, key string, expiration time.Duration, fn func() error) error
```

### LockWithOptions
Acquire a lock with full configuration options and execute a callback function.

```go
func LockWithOptions(ctx context.Context, key string, options LockOptions, fn func() error) error
```

### LockWithRetry
Acquire a lock with retry strategy and execute a callback function.

```go
func LockWithRetry(ctx context.Context, key string, expiration time.Duration, fn func() error, strategy RetryStrategy) error
```

### NewLockFromClient
Create a reusable lock instance.

```go
func NewLockFromClient(ctx context.Context, key string, options LockOptions) (*EtcdLock, error)
```

## Validation

Current automated baseline covers lifecycle idempotency with `go test ./...` and `go vet ./...`. See [VALIDATION.md](./VALIDATION.md) for the exact scope and the remaining live-cluster checks.

## Lifecycle Contract

- Repeated `StartupTasks()` calls on the same live plugin instance are treated as no-op success.
- `CleanupTasks()` is idempotent, but it is also a process-local terminal state for that plugin instance: after cleanup, the instance clears its etcd client handle and `StartupTasks()` will continue to return `etcd lock plugin already destroyed`.
- If you need a fresh start after cleanup, create a new plugin instance instead of trying to restart the cleaned-up instance in place.

## Metrics

This module now registers Prometheus collectors into the default registry on first use, so the metrics appear automatically when the hosting Lynx application exposes Prometheus via the unified metrics handler.

Metric coverage in the current repository:

- `lynx_etcd_lock_operations_total{operation,status}`: counts `lock` / `unlock` / `renew` operations, with `success` / `error` / `conflict` status.
- `lynx_etcd_lock_operation_duration_seconds{operation,status}`: latency histogram for the same critical paths.
- `lynx_etcd_lock_active_locks`: current in-process active lock count.
- `lynx_etcd_lock_total_locks`: cumulative locks added to the renewal manager.
- `lynx_etcd_lock_renewal_attempts_total`: cumulative successful automatic renewals.
- `lynx_etcd_lock_renewal_errors_total`: cumulative automatic renewal errors.
- `lynx_etcd_lock_renewal_skipped_total`: cumulative skipped renewals when the worker pool is saturated.

The current metrics cover in-process operation paths only. They do not replace live-cluster validation for etcd availability, lease behavior, or cross-process contention.

## Dependencies

- `lynx-etcd` - etcd configuration center plugin (required)
- `go.etcd.io/etcd/client/v3` - etcd client library
