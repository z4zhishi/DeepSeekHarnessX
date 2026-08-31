package core

import (
	"context"
	"sync"
	"time"
)

// EventRegistry stores publicly described event schemas.
type EventRegistry struct {
	mu     sync.Mutex
	topics map[string]EventDescriptor
}

// EventDescriptor is the public metadata for one event topic.
type EventDescriptor struct {
	Topic, Description, PayloadSchema string
	SessionScoped, RequiresApproval   bool
}

// NewEventRegistry creates an empty catalog.
func NewEventRegistry() *EventRegistry {
	return &EventRegistry{topics: map[string]EventDescriptor{}}
}

// RegisterTopic adds or replaces a topic descriptor.
func (er *EventRegistry) RegisterTopic(d EventDescriptor) {
	if d.Topic == "" {
		return
	}
	er.mu.Lock()
	defer er.mu.Unlock()
	er.topics[d.Topic] = d
}

// ListTopics returns all registered topics.
func (er *EventRegistry) ListTopics() []EventDescriptor {
	er.mu.Lock()
	defer er.mu.Unlock()
	out := make([]EventDescriptor, 0, len(er.topics))
	for _, d := range er.topics {
		out = append(out, d)
	}
	return out
}

// Describe returns the metadata for a topic.
func (er *EventRegistry) Describe(topic string) (EventDescriptor, bool) {
	er.mu.Lock()
	defer er.mu.Unlock()
	d, ok := er.topics[topic]
	return d, ok
}

// EventBus delivers events with owner-scoped subscriptions.
type EventBus struct {
	mu        sync.Mutex
	handlers  map[string][]sub
	nextID    int64
	nextSubID int64
	nextTopic int64
	registry  *EventRegistry
}

type sub struct {
	id      int64
	ownerID string
	handler func(Event)
}

// NewEventBus creates a minimal but real bus.
func NewEventBus() *EventBus {
	return &EventBus{handlers: map[string][]sub{}}
}

// NewEventBusWithRegistry creates a bus enforcing the supplied topic catalog.
func NewEventBusWithRegistry(registry *EventRegistry) *EventBus {
	if registry == nil {
		registry = NewEventRegistry()
	}
	return &EventBus{handlers: map[string][]sub{}, registry: registry}
}

// Registry returns the event catalog used by this bus.
func (eb *EventBus) Registry() *EventRegistry { return eb.registry }

// Subscribe adds an owner-scoped handler. The returned function removes it.
func (eb *EventBus) Subscribe(topic, ownerID string, handler func(Event)) func() {
	if eb.registry != nil {
		if desc, ok := eb.registry.Describe(topic); ok && desc.RequiresApproval {
			// Approval is enforced by the host before publishing dangerous topics.
			return func() {}
		}
	}
	if topic == "" || ownerID == "" || handler == nil {
		return func() {}
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	eb.nextSubID++
	id := eb.nextSubID
	eb.handlers[topic] = append(eb.handlers[topic], sub{id: id, ownerID: ownerID, handler: handler})
	return func() { eb.removeSub(topic, id) }
}

// Publish delivers the event to matching subscribers synchronously. A
// cancelled context stops delivery before the next handler.
func (eb *EventBus) Publish(ctx context.Context, ev Event) {
	if eb.registry != nil {
		if desc, ok := eb.registry.Describe(ev.Topic); ok && desc.RequiresApproval {
			// Blocked until upstream approval flow is implemented.
			return
		}
	}
	eb.mu.Lock()
	copy := append([]sub{}, eb.handlers[ev.Topic]...)
	eb.mu.Unlock()
	for _, s := range copy {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		if s.handler != nil {
			s.handler(ev)
		}
	}
}

// Unsubscribe removes all handlers registered by ownerID.
func (eb *EventBus) Unsubscribe(ownerID string) {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	for topic, subs := range eb.handlers {
		n := 0
		for _, s := range subs {
			if s.ownerID != ownerID {
				subs[n] = s
				n++
			}
		}
		eb.handlers[topic] = subs[:n]
	}
}

func (eb *EventBus) removeSub(topic string, id int64) {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	subs := eb.handlers[topic]
	for i := range subs {
		if subs[i].id == id {
			eb.handlers[topic] = append(subs[:i], subs[i+1:]...)
			return
		}
	}
}

// Timer manages owner-tracked timers with explicit expiry semantics.
type Timer struct {
	mu       sync.Mutex
	now      func() time.Time
	pending  map[int64]pendingTimer
	cancelCh map[int64]chan struct{}
	nextID   int64
}

type pendingTimer struct {
	id        int64
	ownerID   string
	sessionID string
	deadline  time.Time
}

// NewTimer creates a timer instance for tests and runtime.
func NewTimer(now func() time.Time) *Timer {
	if now == nil {
		now = time.Now
	}
	return &Timer{now: now, pending: map[int64]pendingTimer{}, cancelCh: map[int64]chan struct{}{}}
}

// After registers a delayed callback and returns an owner-bound cancel func.
func (t *Timer) After(ctx context.Context, session SessionContext, ownerID string, delay time.Duration, fn func()) func() {
	t.mu.Lock()
	t.nextID++
	id := t.nextID
	cancel := make(chan struct{})
	t.pending[id] = pendingTimer{id: id, ownerID: ownerID, sessionID: session.ID, deadline: t.now().Add(delay)}
	t.cancelCh[id] = cancel
	t.mu.Unlock()
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-cancel:
			return
		case <-ctx.Done():
			return
		}
		t.mu.Lock()
		_, ok := t.pending[id]
		delete(t.pending, id)
		delete(t.cancelCh, id)
		t.mu.Unlock()
		if ok && fn != nil {
			fn()
		}
	}()
	return func() { t.cancel(id) }
}

// CancelOwner cancels all timers owned by a plugin.
func (t *Timer) CancelOwner(ownerID string) {
	t.mu.Lock()
	ids := make([]int64, 0)
	for id, p := range t.pending {
		if p.ownerID == ownerID {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if ch, ok := t.cancelCh[id]; ok {
			close(ch)
			delete(t.cancelCh, id)
		}
		delete(t.pending, id)
	}
	t.mu.Unlock()
}

// CancelSession cancels all timers bound to a session.
func (t *Timer) CancelSession(session SessionContext) {
	if session.ID == "" {
		return
	}
	t.mu.Lock()
	ids := make([]int64, 0)
	for id, p := range t.pending {
		if p.sessionID == session.ID {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if ch, ok := t.cancelCh[id]; ok {
			close(ch)
			delete(t.cancelCh, id)
		}
		delete(t.pending, id)
	}
	t.mu.Unlock()
}

// Active returns the number of pending timers.
func (t *Timer) Active() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

func (t *Timer) cancel(id int64) {
	t.mu.Lock()
	if ch, ok := t.cancelCh[id]; ok {
		close(ch)
		delete(t.cancelCh, id)
	}
	delete(t.pending, id)
	t.mu.Unlock()
}

// Tick cancels expired timers and removes their entries.
func (t *Timer) Tick() {
	t.mu.Lock()
	now := t.now()
	remaining := map[int64]pendingTimer{}
	cancelled := make([]chan struct{}, 0)
	for id, p := range t.pending {
		if p.deadline.After(now) {
			remaining[id] = p
		} else if ch, ok := t.cancelCh[id]; ok {
			cancelled = append(cancelled, ch)
			delete(t.cancelCh, id)
		}
	}
	t.pending = remaining
	t.mu.Unlock()
	for _, ch := range cancelled {
		close(ch)
	}
}
