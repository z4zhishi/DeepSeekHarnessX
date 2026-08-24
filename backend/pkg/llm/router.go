package llm

import (
	"context"
	"errors"
	"sync"
)

// Router is a thread-safe LlmAdapter whose inner adapter can Swap without
// restarting agents. Stream snapshots the inner pointer and delegates; an
// in-flight stream keeps the adapter it started with.
type Router struct {
	mu    sync.RWMutex
	inner LlmAdapter
}

// NewRouter wraps inner. inner may be nil; Stream then fails until Swap.
func NewRouter(inner LlmAdapter) *Router {
	return &Router{inner: inner}
}

// Swap replaces the inner adapter. Subsequent Stream calls use the new inner;
// in-flight streams are unaffected.
func (r *Router) Swap(inner LlmAdapter) {
	r.mu.Lock()
	r.inner = inner
	r.mu.Unlock()
}

// Inner returns the current inner adapter (may be nil).
func (r *Router) Inner() LlmAdapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.inner
}

// Stream implements LlmAdapter by snapshotting the inner adapter and delegating.
func (r *Router) Stream(ctx context.Context, req ModelRequest) (<-chan StreamChunk, <-chan error) {
	r.mu.RLock()
	inner := r.inner
	r.mu.RUnlock()
	if inner == nil {
		return failStream(errors.New("llm: router has no adapter"))
	}
	return inner.Stream(ctx, req)
}

// SetModel snapshots the inner adapter and, when it exposes SetModel(string),
// forwards the id so a picker change reaches the next Stream call.
func (r *Router) SetModel(model string) {
	r.mu.RLock()
	inner := r.inner
	r.mu.RUnlock()
	if sm, ok := inner.(interface{ SetModel(string) }); ok {
		sm.SetModel(model)
	}
}

// Model snapshots the inner adapter and, when it exposes Model(), returns
// that id. Missing inner/method yields "".
func (r *Router) Model() string {
	r.mu.RLock()
	inner := r.inner
	r.mu.RUnlock()
	if m, ok := inner.(interface{ Model() string }); ok {
		return m.Model()
	}
	return ""
}
