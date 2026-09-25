package provider

import (
	"fmt"
	"sync"
)

// Registry holds providers registered at compile time.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]ModelLister
	disabled  map[string]struct{}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]ModelLister)}
}

// Register adds a provider under its ID. Duplicate IDs are rejected.
func (r *Registry) Register(p ModelLister) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[p.ID()]; ok {
		return fmt.Errorf("provider %q already registered", p.ID())
	}
	r.providers[p.ID()] = p
	return nil
}

// Lookup returns the provider registered under id.
func (r *Registry) Lookup(id string) (ModelLister, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return p, ok
}

// SetEnabled marks a registered provider enabled or disabled.
func (r *Registry) SetEnabled(id string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if enabled {
		if r.disabled == nil {
			return
		}
		delete(r.disabled, id)
		return
	}
	if r.disabled == nil {
		r.disabled = make(map[string]struct{})
	}
	r.disabled[id] = struct{}{}
}

// Enabled reports whether a registered provider is enabled.
// Unknown ids report false.
func (r *Registry) Enabled(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.providers[id]; !ok {
		return false
	}
	_, off := r.disabled[id]
	return !off
}

// EnabledProviders returns all registered providers that are enabled.
func (r *Registry) EnabledProviders() []ModelLister {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelLister, 0, len(r.providers))
	for id, p := range r.providers {
		if _, off := r.disabled[id]; !off {
			out = append(out, p)
		}
	}
	return out
}
