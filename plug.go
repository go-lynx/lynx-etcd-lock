package etcdlock

import (
	"github.com/go-lynx/lynx/pkg/factory"
	"github.com/go-lynx/lynx/plugins"
)

// init registers the etcd distributed-lock plugin with the global factory on
// import, so the plugin manager can discover and load it by name.
func init() {
	factory.GlobalTypedFactory().RegisterPlugin(pluginName, confPrefix, func() plugins.Plugin {
		return NewEtcdLockPlugin()
	})
}
