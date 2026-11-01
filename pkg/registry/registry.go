// Package registry holds the live set of connector adapters. It is swapped
// under a lock by the reloader, so the front always resolves a connector by
// name at request time and never keeps a stale adapter.
package registry

import (
	"sort"
	"sync"

	"github.com/danieliser/capability-broker/pkg/adapter"
)

type Registry struct {
	mu       sync.RWMutex
	adapters map[string]adapter.Adapter
}

func New() *Registry { return &Registry{adapters: map[string]adapter.Adapter{}} }

func (r *Registry) Lookup(name string) (adapter.Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
}

// Names lists connector names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for k := range r.adapters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Set installs or replaces an adapter and returns the one it replaced, if any.
func (r *Registry) Set(name string, a adapter.Adapter) adapter.Adapter {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.adapters[name]
	r.adapters[name] = a
	return old
}

// Remove drops an adapter and returns it.
func (r *Registry) Remove(name string) adapter.Adapter {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.adapters[name]
	delete(r.adapters, name)
	return old
}

// Snapshot returns a copy of the current map (status, health).
func (r *Registry) Snapshot() map[string]adapter.Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]adapter.Adapter, len(r.adapters))
	for k, v := range r.adapters {
		out[k] = v
	}
	return out
}
