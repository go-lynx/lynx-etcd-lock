package etcdlock

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-lynx/lynx"
	"github.com/go-lynx/lynx/log"
	"github.com/go-lynx/lynx/plugins"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Plugin metadata
const (
	pluginName        = "etcd.distributed.lock"
	pluginVersion     = "v1.0.0"
	pluginDescription = "etcd distributed lock plugin for lynx framework"
	confPrefix        = "lynx.etcd-lock"

	// etcd plugin dependency
	etcdPluginName    = "etcd.config.center"
	etcdPluginVersion = "v1.0.0"
)

// PlugEtcdLock represents an etcd distributed lock plugin instance
type PlugEtcdLock struct {
	*plugins.BasePlugin
	client      *clientv3.Client
	initialized int32
	destroyed   int32
	mu          sync.RWMutex
}

// NewEtcdLockPlugin creates a new etcd distributed lock plugin
func NewEtcdLockPlugin() *PlugEtcdLock {
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
	// Declare dependency on etcd config center plugin
	p.AddDependency(plugins.Dependency{
		ID:          plugins.GeneratePluginID("", etcdPluginName, etcdPluginVersion),
		Name:        etcdPluginName,
		Type:        plugins.DependencyTypeRequired,
		Required:    true,
		Description: "Etcd configuration center plugin is required for distributed lock functionality",
	})
	return nil
}

// StartupTasks implements custom startup logic
func (p *PlugEtcdLock) StartupTasks() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if atomic.LoadInt32(&p.initialized) == 1 {
		return fmt.Errorf("etcd lock plugin already initialized")
	}

	// Get etcd client from etcd config center plugin
	// The dependency system ensures etcd plugin is loaded and started first
	manager := lynx.Lynx().GetPluginManager()
	if manager == nil {
		return fmt.Errorf("plugin manager not initialized")
	}

	etcdPlugin := manager.GetPlugin(etcdPluginName)
	if etcdPlugin == nil {
		return fmt.Errorf("etcd config center plugin (%s) not found, please ensure it is configured and loaded", etcdPluginName)
	}

	// Check if etcd plugin has started successfully by checking its status
	if status := etcdPlugin.Status(etcdPlugin); status != plugins.StatusActive {
		return fmt.Errorf("etcd config center plugin is not active (status: %v), please check its initialization", status)
	}

	// Try to get client from etcd plugin
	if plugEtcd, ok := etcdPlugin.(interface{ GetClient() *clientv3.Client }); ok {
		p.client = plugEtcd.GetClient()
		if p.client == nil {
			return fmt.Errorf("etcd client is nil, etcd plugin may not have initialized properly")
		}
	} else {
		return fmt.Errorf("etcd plugin does not implement GetClient() method")
	}

	// Set global client getter
	GetEtcdClient = func() *clientv3.Client {
		return p.client
	}

	atomic.StoreInt32(&p.initialized, 1)
	log.Infof("Etcd lock plugin started successfully with etcd client connection")
	return nil
}

// CleanupTasks implements custom cleanup logic
func (p *PlugEtcdLock) CleanupTasks() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if atomic.LoadInt32(&p.destroyed) == 1 {
		return fmt.Errorf("etcd lock plugin already destroyed")
	}

	// Shutdown lock manager
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Shutdown(ctx); err != nil {
		log.Warnf("Failed to shutdown lock manager: %v", err)
	}

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
