package etcdlock

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-lynx/lynx/log"
	"github.com/go-lynx/lynx/plugins"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	pluginName        = "etcd.distributed.lock"
	pluginVersion     = "v1.6.3"
	pluginDescription = "etcd distributed lock plugin for lynx framework"
	confPrefix        = "lynx.etcd-lock"
)

// PlugEtcdLock is the distributed-lock plugin. It borrows the live etcd client
// from the config-centre plugin rather than dialing its own connection.
type PlugEtcdLock struct {
	*plugins.BasePlugin
	client      *clientv3.Client
	rt          plugins.Runtime
	initialized int32
	destroyed   int32
	mu          sync.RWMutex
}

// NewEtcdLockPlugin returns an uninitialized lock plugin. Its weight is one below
// the config centre so the centre (which owns the etcd client) starts first.
func NewEtcdLockPlugin() *PlugEtcdLock {
	ensureMetricsRegistered()
	return &PlugEtcdLock{
		BasePlugin: plugins.NewBasePlugin(
			plugins.GeneratePluginID("", pluginName, pluginVersion),
			pluginName,
			pluginDescription,
			pluginVersion,
			confPrefix,
			math.MaxInt-1,
		),
	}
}

// InitializeResources fetches the shared etcd client from the config-centre
// plugin and installs the client provider used to build locks.
func (p *PlugEtcdLock) InitializeResources(rt plugins.Runtime) error {
	etcdPlugin, err := rt.GetSharedResource("etcd.config.center")
	if err != nil {
		return fmt.Errorf("etcd config center plugin not found, please load it first: %w", err)
	}
	if etcdPlugin == nil {
		return fmt.Errorf("etcd config center plugin resource is nil")
	}

	if plugEtcd, ok := etcdPlugin.(interface{ GetClient() *clientv3.Client }); ok {
		p.client = plugEtcd.GetClient()
		if p.client == nil {
			return fmt.Errorf("etcd client is nil")
		}
	} else {
		return fmt.Errorf("etcd plugin does not provide client")
	}

	p.rt = rt.WithPluginContext(pluginName)
	setClientProvider(func() ClientProvider {
		return clientProviderFunc(func(ctx context.Context) (*clientv3.Client, error) {
			p.mu.RLock()
			defer p.mu.RUnlock()
			if p.client == nil {
				return nil, fmt.Errorf("etcd client is nil")
			}
			return p.client, nil
		})
	})

	log.Infof("Etcd lock plugin initialized successfully")
	return nil
}

// GetDependencies ensures the lock plugin starts after the etcd config-center plugin.
func (p *PlugEtcdLock) GetDependencies() []plugins.Dependency {
	return []plugins.Dependency{
		{
			Name:        "etcd.config.center",
			Type:        plugins.DependencyTypeRequired,
			Required:    true,
			Description: "Etcd config center plugin provides the live etcd client",
		},
	}
}

// StartupTasks publishes the lock provider as a runtime resource so other
// plugins can acquire locks. Idempotent and safe once the client is present.
func (p *PlugEtcdLock) StartupTasks() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if atomic.LoadInt32(&p.destroyed) == 1 {
		return fmt.Errorf("etcd lock plugin already destroyed")
	}
	if atomic.LoadInt32(&p.initialized) == 1 {
		return nil
	}

	if p.client == nil {
		return fmt.Errorf("etcd client is nil")
	}

	if p.rt != nil {
		lockProvider := GetProvider()
		for _, resourceName := range []string{pluginName, pluginName + ".provider"} {
			if err := p.rt.RegisterSharedResource(resourceName, lockProvider); err != nil {
				log.Warnf("failed to register etcd lock shared resource %s: %v", resourceName, err)
			}
		}
		if err := p.rt.RegisterPrivateResource("provider", lockProvider); err != nil {
			log.Warnf("failed to register etcd lock private provider resource: %v", err)
		}
		if err := p.rt.RegisterPrivateResource("client_provider", GetClientProvider()); err != nil {
			log.Warnf("failed to register etcd lock private client provider resource: %v", err)
		}
	}

	atomic.StoreInt32(&p.initialized, 1)
	log.Infof("Etcd lock plugin started successfully")
	return nil
}

// CleanupTasks drains the renewal manager (waiting up to 10s for in-flight locks
// to release), clears the borrowed client, and tears down the provider.
func (p *PlugEtcdLock) CleanupTasks() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if atomic.LoadInt32(&p.destroyed) == 1 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Shutdown(ctx); err != nil {
		log.Warnf("Failed to shutdown lock manager: %v", err)
	}

	p.client = nil
	p.rt = nil
	resetClientProvider()
	atomic.StoreInt32(&p.initialized, 0)
	atomic.StoreInt32(&p.destroyed, 1)
	log.Infof("Etcd lock plugin cleanup completed")
	return nil
}

// CheckHealth reports healthy only when initialized and the etcd client is present.
func (p *PlugEtcdLock) CheckHealth() error {
	if atomic.LoadInt32(&p.initialized) == 0 {
		return fmt.Errorf("etcd lock plugin not initialized")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.client == nil {
		return fmt.Errorf("etcd client is nil")
	}
	return nil
}
