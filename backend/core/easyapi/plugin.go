package easyapi

import "dsh-go/core"

// Plugin is a thin, opt-in convenience layer on top of the Core API. It is a
// real plugin that exposes simplified helpers for context, events and tasks.
type Plugin struct {
	safe   core.SafeContext
	unsafe core.UnsafeContext
	events core.EventAPI
	tasks  core.TaskAPI
}

// New creates a concrete EasyAPI instance from real Core services.
func New(safe core.SafeContext, unsafe core.UnsafeContext, events core.EventAPI, tasks core.TaskAPI) *Plugin {
	return &Plugin{safe: safe, unsafe: unsafe, events: events, tasks: tasks}
}

// SafeContext returns the real Core-backed safe context adapter.
func (p *Plugin) SafeContext() core.SafeContext { return p.safe }

// UnsafeContext returns the real Core-backed unsafe context adapter.
func (p *Plugin) UnsafeContext() core.UnsafeContext { return p.unsafe }

// Events returns the real Core-backed event API.
func (p *Plugin) Events() core.EventAPI { return p.events }

// Tasks returns the real Core-backed task API.
func (p *Plugin) Tasks() core.TaskAPI { return p.tasks }
