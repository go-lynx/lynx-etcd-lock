package etcdlock

// IsContextAware asserts that the plugin's lifecycle genuinely observes context
// cancellation. The core BasePlugin drives StartContext/StopContext and routes
// into StartupTasksContext / CleanupTasksContext (see plugin.go), which check
// ctx between registration steps and bind the lock-manager drain to ctx.
func (p *PlugEtcdLock) IsContextAware() bool {
	return true
}
