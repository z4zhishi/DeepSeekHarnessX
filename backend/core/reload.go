package core

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

var (
	ErrNoOwner   = errors.New("reload: no owner configured")
	ErrNoFactory = errors.New("reload: no factory configured")
)

// Factory builds a replacement owner from a detached configuration snapshot.
type Factory func(snapshot map[string]any) (any, error)

// ReloadManager owns the active runtime owner and its configuration. Reloads
// are serialized and failed builds leave the active state untouched.
type ReloadManager struct {
	// mu protects the active state and factory. reloadMu serializes the
	// build-and-commit transaction without holding mu during user code.
	mu       sync.RWMutex
	reloadMu sync.Mutex
	owner    any
	config   map[string]any
	factory  Factory
}

// NewReloadManager creates a manager with an optional initial owner and factory.
func NewReloadManager(owner any, config map[string]any, factory Factory) *ReloadManager {
	return &ReloadManager{owner: owner, config: cloneMap(config), factory: factory}
}

// SetFactory changes the factory used by subsequent reloads.
func (m *ReloadManager) SetFactory(factory Factory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.factory = factory
}

// Owner returns the active opaque owner.
func (m *ReloadManager) Owner() any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.owner
}

// Snapshot returns a detached copy of the active configuration.
func (m *ReloadManager) Snapshot() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneMap(m.config)
}

// Reload builds and atomically installs a replacement. The previous owner and
// configuration remain the rollback state on every factory failure. The active
// owner must implement io.Closer for graceful teardown.
func (m *ReloadManager) Reload(config map[string]any) (any, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	m.mu.RLock()
	factory := m.factory
	m.mu.RUnlock()
	if factory == nil {
		return nil, ErrNoFactory
	}

	candidateConfig := cloneMap(config)

	// Capture the current snapshot outside any commit lock.
	m.mu.RLock()
	previous := m.owner
	m.mu.RUnlock()

	candidate, err := factory(cloneMap(candidateConfig))
	if err != nil {
		return previous, fmt.Errorf("reload: build candidate: %w", err)
	}

	// Commit the candidate while holding the write lock so that readers see a
	// consistent owner/config pair at all times. Previous values are always
	// available for rollback.
	m.mu.Lock()
	m.owner = candidate
	m.config = candidateConfig
	m.mu.Unlock()

	// Close the previous owner after commit so that in-flight readers can
	// drain safely before seeing teardown errors. This ordering matches the
	// plan requirement: candidate ready -> commit -> teardown old.
	if c, ok := previous.(io.Closer); ok && c != nil {
		if err := c.Close(); err != nil {
			return candidate, fmt.Errorf("reload: close previous owner: %w", err)
		}
	}
	return candidate, nil
}

// ReloadCurrent rebuilds the active owner from its current configuration.
func (m *ReloadManager) ReloadCurrent() (any, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	m.mu.RLock()
	config := cloneMap(m.config)
	m.mu.RUnlock()
	return m.reloadLocked(config)
}

func (m *ReloadManager) reloadLocked(config map[string]any) (any, error) {
	m.mu.RLock()
	factory := m.factory
	previous := m.owner
	m.mu.RUnlock()
	if factory == nil {
		return nil, ErrNoFactory
	}

	candidateConfig := cloneMap(config)
	candidate, err := factory(cloneMap(candidateConfig))
	if err != nil {
		return previous, fmt.Errorf("reload: build candidate: %w", err)
	}

	// Commit both values together so readers never observe a mixed state.
	m.mu.Lock()
	m.owner = candidate
	m.config = candidateConfig
	m.mu.Unlock()

	if c, ok := previous.(io.Closer); ok && c != nil {
		if err := c.Close(); err != nil {
			return candidate, fmt.Errorf("reload: close previous owner: %w", err)
		}
	}
	return candidate, nil
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = cloneValue(v)
	}
	return dst
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = cloneValue(x[i])
		}
		return out
	default:
		return v
	}
}
