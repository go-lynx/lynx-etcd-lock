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

// Plugin metadata
const (
	pluginName        = "etcd.distributed.lock"
	pluginVersion     = "v1.6.0-beta"
	pluginDescription = "etcd distributed lock plugin for lynx framework"
	confPrefix        = "lynx.etcd-lock"
)

// PlugEtcdLock represents an etcd distributed lock plugin instance
type PlugEtcdLock struct {
	*plugins.BasePlugin
	client      *clientv3.Client
	rt          plugins.Runtime
	initialized int32
	destroyed   int32
	mu          sync.RWMutex
}

// NewEtcdLockPlugin creates a new etcd distributed lock plugin
func NewEtcdLockPlugin() *PlugEtcdLock {
	ensureMetricsRegistered()
	return &PlugEtcdLock{
		BasePlugin: plugins.NewBasePlugin(
			plugins.GeneratePluginID("", pluginName, pluginVersion),
			pluginName,
			pluginDescription,
			pluginVersion,
			confPrefix,
			math.MaxInt-1, // Lower priority than config center
		),
	}
}

// InitializeResources implements custom initialization logic
func (p *PlugEtcdLock) InitializeResources(rt plugins.Runtime) error {
	// Get the started etcd plugin instance from the shared runtime.
	etcdPlugin, err := rt.GetSharedResource("etcd.config.center")
	if err != nil {
		return fmt.Errorf("etcd config center plugin not found, please load it first: %w", err)
	}
	if etcdPlugin == nil {
		return fmt.Errorf("etcd config center plugin resource is nil")
	}

	// Try to get client from etcd plugin
	if plugEtcd, ok := etcdPlugin.(interface{ GetClient() *clientv3.Client }); ok {
		p.client = plugEtcd.GetClient()
		if p.client == nil {
			return fmt.Errorf("etcd client is nil")
		}
	} else {
		return fmt.Errorf("etcd plugin does not provide client")
	}

	p.rt = rt.WithPluginContext(pluginName)
	GetClientProvider = func() ClientProvider {
		return clientProviderFunc(func(ctx context.Context) (*clientv3.Client, error) {
			p.mu.RLock()
			defer p.mu.RUnlock()
			if p.client == nil {
				return nil, fmt.Errorf("etcd client is nil")
			}
			return p.client, nil
		})
	}

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

// StartupTasks implements custom startup logic
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

// CleanupTasks implements custom cleanup logic
func (p *PlugEtcdLock) CleanupTasks() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if atomic.LoadInt32(&p.destroyed) == 1 {
		return nil
	}

	// Shutdown lock manager
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Shutdown(ctx); err != nil {
		log.Warnf("Failed to shutdown lock manager: %v", err)
	}

	p.client = nil
	p.rt = nil
	GetClientProvider = func() ClientProvider { return nil }
	GetEtcdClient = func() *clientv3.Client { return nil }
	atomic.StoreInt32(&p.initialized, 0)
	atomic.StoreInt32(&p.destroyed, 1)
	log.Infof("Etcd lock plugin cleanup completed")
	return nil
}

// CheckHealth implements health check
func (p *PlugEtcdLock) CheckHealth() error {
	if atomic.LoadInt32(&p.initialized) == 0 {
		return fmt.Errorf("etcd lock plugin not initialized")
	}
	if p.client == nil {
		return fmt.Errorf("etcd client is nil")
	}
	return nil
}
