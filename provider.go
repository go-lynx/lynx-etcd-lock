package etcdlock

import (
	"context"
	"fmt"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// ClientProvider resolves the current etcd client on demand.
type ClientProvider interface {
	Client(ctx context.Context) (*clientv3.Client, error)
}

type clientProviderFunc func(ctx context.Context) (*clientv3.Client, error)

func (f clientProviderFunc) Client(ctx context.Context) (*clientv3.Client, error) {
	return f(ctx)
}

// Provider exposes etcd-lock through an injectable facade while resolving the underlying etcd
// client via a stable provider on each call.
type Provider interface {
	NewLock(ctx context.Context, key string, options LockOptions) (*EtcdLock, error)
	Lock(ctx context.Context, key string, expiration time.Duration, fn func() error) error
	LockWithOptions(ctx context.Context, key string, options LockOptions, fn func() error) error
	// LockWithOptionsCtx is like LockWithOptions but passes a lock-scoped context
	// to fn; the context is cancelled when the lock is permanently lost so fn can
	// abort its critical section gracefully via ctx.Err().
	LockWithOptionsCtx(ctx context.Context, key string, options LockOptions, fn func(context.Context) error) error
}

type provider struct{}

var (
	clientProviderMu sync.RWMutex
	clientProviderFn = func() ClientProvider {
		return nil
	}
)

var GetClientProvider = func() ClientProvider {
	clientProviderMu.RLock()
	defer clientProviderMu.RUnlock()
	return clientProviderFn()
}

func setClientProvider(fn func() ClientProvider) {
	if fn == nil {
		fn = func() ClientProvider { return nil }
	}
	clientProviderMu.Lock()
	clientProviderFn = fn
	clientProviderMu.Unlock()
}

func resetClientProvider() {
	setClientProvider(func() ClientProvider {
		return nil
	})
}

// GetProvider returns the injectable etcd lock facade.
func GetProvider() Provider {
	return provider{}
}

func (provider) NewLock(ctx context.Context, key string, options LockOptions) (*EtcdLock, error) {
	return NewLockFromClient(ctx, key, options)
}

func (provider) Lock(ctx context.Context, key string, expiration time.Duration, fn func() error) error {
	return Lock(ctx, key, expiration, fn)
}

func (provider) LockWithOptions(ctx context.Context, key string, options LockOptions, fn func() error) error {
	return LockWithOptions(ctx, key, options, fn)
}

func (provider) LockWithOptionsCtx(ctx context.Context, key string, options LockOptions, fn func(context.Context) error) error {
	return LockWithOptionsCtx(ctx, key, options, fn)
}

func resolveClientProvider() (ClientProvider, error) {
	provider := GetClientProvider()
	if provider == nil {
		return nil, fmt.Errorf("etcd client provider not found")
	}
	return provider, nil
}
