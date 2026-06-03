// Package etcdlock provides a production-grade distributed lock plugin for the
// Lynx framework backed by etcd leases.
//
// # Overview
//
// A distributed lock is acquired by writing an etcd key under a shared prefix
// using an atomic compare-and-swap transaction (CreateRevision == 0).  The key
// is bound to an etcd lease so that the lock is automatically released when the
// lease expires, preventing deadlocks from crashed holders.
//
// # Quick start
//
//	err := etcdlock.Lock(ctx, "my-resource", 30*time.Second, func() error {
//	    // critical section
//	    return doWork()
//	})
//
// For finer control use LockWithOptions or create a reusable *EtcdLock via
// NewLock / NewLockFromClient.
//
// # Automatic renewal
//
// When LockOptions.RenewalEnabled is true (the default) the lock is added to the
// global lockManager which periodically calls KeepAliveOnce on each managed lease
// before it would expire.  The renewal threshold (default 0.3) controls how early
// renewal is attempted: a 30 s lock with threshold 0.3 triggers renewal when fewer
// than 9 s remain.
//
// # Observability
//
// Prometheus metrics are registered on first use (ensureMetricsRegistered) and
// expose per-operation counters and latency histograms under the
// lynx_etcd_lock_* namespace.
//
// # Plugin integration
//
// Import the package to auto-register the "etcd.distributed.lock" plugin via the
// Lynx plugin factory.  The plugin declares a required dependency on
// "etcd.config.center" so the framework starts them in the correct order and
// injects the live etcd client automatically.
package etcdlock
