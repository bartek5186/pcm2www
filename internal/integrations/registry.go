// internal/integrations/registry.go
package integrations

import "sync"

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

func Register(name string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[name] = f
}

func Get(name string) (Factory, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	f, ok := registry[name]
	return f, ok
}

func All() map[string]Factory {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make(map[string]Factory, len(registry))
	for k, v := range registry {
		out[k] = v
	}
	return out
}
