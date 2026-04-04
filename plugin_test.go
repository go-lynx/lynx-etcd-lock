package etcdlock

import (
	"testing"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestStartupTasksIsIdempotent(t *testing.T) {
	originalManager := globalLockManager
	t.Cleanup(func() {
		globalLockManager = originalManager
	})
	globalLockManager = &lockManager{locks: make(map[string]*EtcdLock)}

	plugin := NewEtcdLockPlugin()
	plugin.client = &clientv3.Client{}

	if err := plugin.StartupTasks(); err != nil {
		t.Fatalf("first startup failed: %v", err)
	}
	if err := plugin.StartupTasks(); err != nil {
		t.Fatalf("second startup should be idempotent, got: %v", err)
	}
}

func TestCleanupTasksIsIdempotent(t *testing.T) {
	originalManager := globalLockManager
	originalGetter := GetEtcdClient
	t.Cleanup(func() {
		globalLockManager = originalManager
		GetEtcdClient = originalGetter
	})

	globalLockManager = &lockManager{locks: make(map[string]*EtcdLock)}
	GetEtcdClient = func() *clientv3.Client { return &clientv3.Client{} }

	plugin := NewEtcdLockPlugin()
	plugin.client = &clientv3.Client{}

	if err := plugin.StartupTasks(); err != nil {
		t.Fatalf("startup failed: %v", err)
	}
	if err := plugin.CleanupTasks(); err != nil {
		t.Fatalf("first cleanup failed: %v", err)
	}
	if err := plugin.CleanupTasks(); err != nil {
		t.Fatalf("second cleanup should be idempotent, got: %v", err)
	}
	if err := plugin.CheckHealth(); err == nil {
		t.Fatal("expected health check to fail after cleanup")
	}
	if client := GetEtcdClient(); client != nil {
		t.Fatalf("expected global etcd client getter to be cleared, got %#v", client)
	}
}

func TestStartupAfterCleanupReturnsError(t *testing.T) {
	originalManager := globalLockManager
	originalGetter := GetEtcdClient
	t.Cleanup(func() {
		globalLockManager = originalManager
		GetEtcdClient = originalGetter
	})
	globalLockManager = &lockManager{locks: make(map[string]*EtcdLock)}

	plugin := NewEtcdLockPlugin()
	plugin.client = &clientv3.Client{}

	if err := plugin.CleanupTasks(); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if err := plugin.StartupTasks(); err == nil {
		t.Fatal("expected startup after cleanup to fail")
	}
}
