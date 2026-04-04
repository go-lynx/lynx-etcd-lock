# Validation

## Automated Baseline

Current workspace baseline:

```bash
go test ./...
go vet ./...
```

Output:

```text
ok  	github.com/go-lynx/lynx/plugins/etcd-lock
```

## What This Means

- Automated coverage now verifies repeated `StartupTasks()` / `CleanupTasks()` calls, plus the terminal lifecycle contract that forbids re-starting the same plugin instance after cleanup.
- Prometheus metrics are now registered into the default registry and exercised by tests, covering `lock` / `unlock` / `renew` latency and counts plus in-process renewal gauges/counters.
- Lock acquisition, renewal, retry, and shutdown behavior against a real etcd cluster still rely on manual integration verification.

## Exported Metrics

- `lynx_etcd_lock_operations_total{operation,status}`
- `lynx_etcd_lock_operation_duration_seconds{operation,status}`
- `lynx_etcd_lock_active_locks`
- `lynx_etcd_lock_total_locks`
- `lynx_etcd_lock_renewal_attempts_total`
- `lynx_etcd_lock_renewal_errors_total`
- `lynx_etcd_lock_renewal_skipped_total`

## Recommended Manual Smoke Checks

- Start Lynx with the `lynx-etcd` plugin initialized and verify `GetEtcdClient` is wired before using this package.
- Run two contenders on the same key and confirm only one callback enters the critical section at a time.
- For long-running callbacks, verify renewal keeps the lease alive and that `Shutdown()` stops renewal work cleanly.
- Repeat plugin startup/cleanup in the managed Lynx lifecycle and confirm duplicate calls are treated as no-op success while the cleaned-up instance remains terminal and must be recreated before the next startup.
- Scrape the hosting application's metrics endpoint and confirm the exported families above move when you exercise acquire, release, and renewal paths against a live etcd-backed runtime.
