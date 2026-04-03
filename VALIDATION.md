# Validation

## Automated Baseline

Current workspace baseline:

```bash
go test ./...
```

Output:

```text
?    github.com/go-lynx/lynx/plugins/etcd-lock    [no test files]
```

## What This Means

- This module currently has no committed Go test files.
- Lock acquisition, renewal, retry, and shutdown behavior still rely on manual integration verification against a live etcd-backed Lynx runtime.

## Recommended Manual Smoke Checks

- Start Lynx with the `lynx-etcd` plugin initialized and verify `GetEtcdClient` is wired before using this package.
- Run two contenders on the same key and confirm only one callback enters the critical section at a time.
- For long-running callbacks, verify renewal keeps the lease alive and that `Shutdown()` stops renewal work cleanly.
