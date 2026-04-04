package etcdlock

import (
	"context"
	"fmt"
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
}

type provider struct{}

var GetClientProvider = func() ClientProvider {
	return nil
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

func resolveClientProvider() (ClientProvider, error) {
	provider := GetClientProvider()
	if provider == nil {
		return nil, fmt.Errorf("etcd client provider not found")
	}
	return provider, nil
}
