package core

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Registry owns discovered plugins, active handles and capability metadata.
type Registry struct {
	mu      sync.Mutex
	plugins map[string]Plugin
	handles map[string]Handle
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{plugins: map[string]Plugin{}, handles: map[string]Handle{}}
}

// Register adds a discovered plugin. Duplicate registrations replace the prior entry.
func (r *Registry) Register(p Plugin) error {
	if p == nil {
		return errNilPlugin
	}
	meta := p.Metadata()
	if meta.ID == "" {
		return errEmptyPluginID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins[meta.ID] = p
	return nil
}

// Load enables all discovered plugins and keeps their handles. It preserves the
// original API and uses a background context for plugin initialization.
func (r *Registry) Load(svc *Context) error {
	return r.LoadContext(context.Background(), svc)
}

// LoadContext enables all discovered plugins with an explicit cancellation
// context. Plugins are loaded without holding the registry lock.
func (r *Registry) LoadContext(ctx context.Context, svc *Context) error {
	snap := r.snapshotPlugins()

	ordered := make([]Plugin, 0, len(snap))
	for _, p := range snap {
		ordered = append(ordered, p)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if len(ordered[i].Metadata().Dependencies) == 0 && len(ordered[j].Metadata().Dependencies) > 0 {
			return true
		}
		if len(ordered[j].Metadata().Dependencies) == 0 && len(ordered[i].Metadata().Dependencies) > 0 {
			return false
		}
		return ordered[i].Metadata().ID < ordered[j].Metadata().ID
	})

	type loaded struct {
		id     string
		handle Handle
	}
	var succeeded []loaded

	for _, p := range ordered {
		meta := p.Metadata()
		h, err := p.Load(ctx, svc)
		if err != nil {
			for i := len(succeeded) - 1; i >= 0; i-- {
				_ = succeeded[i].handle.Close()
			}
			return fmt.Errorf("core: load plugin %s: %w", meta.ID, err)
		}
		succeeded = append(succeeded, loaded{id: meta.ID, handle: h})
	}

	r.mu.Lock()
	old := make([]Handle, 0)
	for _, s := range succeeded {
		if h, ok := r.handles[s.id]; ok && h != nil {
			old = append(old, h)
		}
		r.handles[s.id] = s.handle
	}
	r.mu.Unlock()
	for _, h := range old {
		_ = h.Close()
	}
	return nil
}

func (r *Registry) snapshotPlugins() map[string]Plugin {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Plugin, len(r.plugins))
	for id, p := range r.plugins {
		out[id] = p
	}
	return out
}

func (r *Registry) Unload() {
	r.mu.Lock()
	items := make([]Handle, 0, len(r.handles))
	for _, h := range r.handles {
		items = append(items, h)
	}
	r.handles = map[string]Handle{}
	r.mu.Unlock()
	for _, h := range items {
		if h != nil {
			_ = h.Close()
		}
	}
}

// Describe returns capability metadata for the registered plugins.
func (r *Registry) Describe() []Metadata {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Metadata, 0, len(r.plugins))
	for _, p := range r.plugins {
		out = append(out, p.Metadata())
	}
	return out
}

// FindPlugin returns the registered plugin with the given ID, if any.
func (r *Registry) FindPlugin(id string) (Plugin, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.plugins[id]
	return p, ok
}

// HandleOf returns the active handle for a loaded plugin.
func (r *Registry) HandleOf(id string) (Handle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.handles[id]
	return h, ok
}

// Has reports whether a plugin is loaded.
func (r *Registry) Has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.handles[id]
	return ok
}

// EqualHandle reports whether two handles are identical.
func EqualHandle(a, b Handle) bool {
	return reflect.DeepEqual(a, b)
}
