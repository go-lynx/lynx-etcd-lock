package etcdlock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-lynx/lynx/plugins"
)

func TestPlugEtcdLock_HasTrueContextLifecycle(t *testing.T) {
	p := NewEtcdLockPlugin()
	if !plugins.HasTrueContextLifecycle(p) {
		t.Fatal("expected plugin to report a true context lifecycle")
	}
	if !plugins.SupportsContextSteps(p) {
		t.Fatal("expected plugin to expose context-aware step hooks")
	}
	// The outer plugin type must be what the framework sees, not a promoted
	// method from an embedded base that would bypass the plugin's own wrapper.
	var _ plugins.ContextStartupTasker = p
	var _ plugins.ContextCleanupTasker = p
	var _ plugins.ContextAwareness = p
	if !p.IsContextAware() {
		t.Fatal("expected IsContextAware() to be true")
	}
}

func TestPlugEtcdLock_StartupTasksContext_AlreadyCanceled(t *testing.T) {
	p := NewEtcdLockPlugin()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := p.StartupTasksContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("startup did not return promptly: %v", elapsed)
	}
}

func TestPlugEtcdLock_CleanupTasksContext_AlreadyCanceled(t *testing.T) {
	p := NewEtcdLockPlugin()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := p.CleanupTasksContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
