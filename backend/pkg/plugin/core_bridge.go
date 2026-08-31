package plugin

import (
	"context"

	"dsh-go/core"
	"dsh-go/pkg/tools"
)

// CoreBridge exposes the existing plugin registries through the stable core
// plugin API. It does not copy or shadow registrations: all fields point at
// the host's live registries and the event adapter forwards synchronously.
//
// 非产品路径：CoreBridge 只是 core API（dsh-go/core）到本包 Registry 在用注册
// 表的适配器，不是第二个插件宿主，也不产生第二条生产装配路径。生产宿主仍是
// 本包 Registry（cmd/dsh/main.go 的 build() 是唯一装配点）；core 包自身的
// core.NewRegistry 不被任何 cmd 构造，属非生产实现（W9-b 隔离）。目前仓库内
// 没有任何能力消费 Capabilities.Bridge，该字段仅为未来单一宿主评估预留。
type CoreBridge struct {
	Tools    *tools.ToolRegistry
	Commands *tools.CommandRegistry
	Events   *EventBus
}

// NewCoreBridge creates a bridge over the host registries. A nil command
// registry uses the ToolRegistry's shared command registry; a nil event bus is
// replaced with a real empty bus.
func NewCoreBridge(toolReg *tools.ToolRegistry, commands *tools.CommandRegistry, events *EventBus) *CoreBridge {
	if commands == nil && toolReg != nil {
		commands = toolReg.Commands
	}
	if events == nil {
		events = NewEventBus()
	}
	return &CoreBridge{Tools: toolReg, Commands: commands, Events: events}
}

// Context returns a core context using this bridge's live event bus. Existing
// services in base (session, task and context services) are preserved.
func (b *CoreBridge) Context(base *core.Context) *core.Context {
	return b.ContextOwned("", base)
}

// ContextOwned returns a core context scoped to an explicit owner, so
// core-backed plugins get correct owner-bound event subscriptions.
func (b *CoreBridge) ContextOwned(ownerID string, base *core.Context) *core.Context {
	if base == nil {
		base = &core.Context{}
	}
	copy := *base
	copy.Events = eventAPI{bus: b.Events, owner: ownerID}
	return &copy
}

type eventAPI struct {
	bus   *EventBus
	owner string
}

func (a eventAPI) Subscribe(topic, ownerID string, handler func(core.Event)) func() {
	if a.bus == nil || handler == nil {
		return func() {}
	}
	owner := ownerID
	if owner == "" {
		owner = a.owner
	}
	if owner == "" {
		return func() {}
	}
	return a.bus.OnOwned(owner, topic, func(ev Event) {
		handler(core.Event{Topic: ev.Topic, Payload: ev.Payload})
	})
}

func (a eventAPI) Publish(ctx context.Context, ev core.Event) {
	if a.bus == nil {
		return
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
	a.bus.Emit(ev.Topic, ev.Payload)
}

// MountCore loads a core plugin with this bridge's live registries and returns
// its handle. ownerID is recorded for owner-scoped event subscriptions.
func (b *CoreBridge) MountCore(ctx context.Context, p core.Plugin, base *core.Context) (core.Handle, error) {
	return b.MountCoreOwned(ctx, "", p, base)
}

func (b *CoreBridge) MountCoreOwned(ctx context.Context, ownerID string, p core.Plugin, base *core.Context) (core.Handle, error) {
	if p == nil {
		return nil, nil
	}
	return p.Load(ctx, b.ContextOwned(ownerID, base))
}
