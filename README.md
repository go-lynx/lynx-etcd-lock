# lynx-etcd-lock

Strongly consistent distributed lock plugin based on etcd for implementing distributed locks using etcd in the Lynx framework.

## Features

- ✅ Strongly consistent distributed lock based on etcd
- ✅ Automatic renewal support
- ✅ Retry strategy
- ✅ Lock timeout control
- ✅ Health check
- ✅ Metrics monitoring
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

## Dependencies

- `lynx-etcd` - etcd configuration center plugin (required)
- `go.etcd.io/etcd/client/v3` - etcd client library
