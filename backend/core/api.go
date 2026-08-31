package core

import "context"

// Plugin describes a loadable capability provider. Implementations must release
// every resource acquired through ctx when Close is called.
type Plugin interface {
	Metadata() Metadata
	Load(context.Context, *Context) (Handle, error)
}

// Handle represents one enabled plugin instance. Close is idempotent.
type Handle interface {
	Close() error
}

// NopHandle is a real handle wrapper for simple lifecycle cleanup without
// extra resources.
type NopHandle struct{ CloseFunc func() }

func (h NopHandle) Close() error {
	if h.CloseFunc != nil {
		h.CloseFunc()
	}
	return nil
}

// Metadata describes a plugin and its declared capabilities.
type Metadata struct {
	ID, Version, APIVersion, Description, Author string
	Required                                     bool
	Dependencies                                 []string
	Capabilities                                 []Capability
}

// Capability describes one discoverable plugin capability.
type Capability struct {
	ID, Type, Description string
	Version               string
	Permissions           []string
	Sync                  bool
}

// Context is the host services exposed to a plugin.
type Context struct {
	Session SessionContext
	Events  EventAPI
	Tasks   TaskAPI
	Safe    SafeContext
	Unsafe  UnsafeContext
}

// SessionContext identifies the execution scope of a plugin call.
type SessionContext struct{ ID, TurnID, StepID string }

// EventAPI is the minimal public event contract; implementations may add
// discovery and filtering while preserving these semantics.
type EventAPI interface {
	Subscribe(topic, ownerID string, handler func(Event)) func()
	Publish(context.Context, Event)
}

// Event is a versioned event delivered to a plugin.
type Event struct {
	Topic, Version string
	Session        SessionContext
	Payload        any
}

// TaskAPI schedules work without requiring plugins to manage goroutines.
type TaskAPI interface {
	CancelOwner(ownerID string)
	CancelSession(session SessionContext)
	Active() int
	Tick()
}
