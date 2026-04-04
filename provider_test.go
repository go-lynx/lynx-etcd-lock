package etcdlock

import (
	"context"
	"testing"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type fakeClientProvider struct {
	client *clientv3.Client
}

func (p fakeClientProvider) Client(context.Context) (*clientv3.Client, error) {
	return p.client, nil
}

func TestNewLock_UsesClientProvider(t *testing.T) {
	lock, err := NewLock(context.Background(), fakeClientProvider{client: &clientv3.Client{}}, "order-1", DefaultLockOptions)
	if err != nil {
		t.Fatalf("expected provider-backed etcd lock, got error: %v", err)
	}
	client, err := lock.currentClient(context.Background())
	if err != nil {
		t.Fatalf("expected current client from provider, got error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil etcd client from provider")
	}
}

func TestEtcdLock_CurrentClientFollowsProviderAfterSwap(t *testing.T) {
	first := &clientv3.Client{}
	second := &clientv3.Client{}
	provider := &fakeClientProvider{client: first}

	lock, err := NewLock(context.Background(), provider, "order-2", DefaultLockOptions)
	if err != nil {
		t.Fatalf("expected provider-backed etcd lock, got error: %v", err)
	}

	client, err := lock.currentClient(context.Background())
	if err != nil {
		t.Fatalf("expected initial current client, got error: %v", err)
	}
	if client != first {
		t.Fatalf("expected first client, got %v", client)
	}

	provider.client = second

	client, err = lock.currentClient(context.Background())
	if err != nil {
		t.Fatalf("expected swapped current client, got error: %v", err)
	}
	if client != second {
		t.Fatalf("expected second client after provider swap, got %v", client)
	}
}
