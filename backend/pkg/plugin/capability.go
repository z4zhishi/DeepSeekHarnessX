package plugin

import (
	"dsh-go/pkg/llm"
	"dsh-go/pkg/tools"
)

// Capability 是内置（编译期）插件的契约（目标架构 §capability.go）。
// 一个内置插件实现 Capability，并注册工具/命令；可选地通过可选接口
// 暴露更深的运行时集成（LLM 适配器、会话存储等）。
//
// 上手规则（写进 docs/plugin-onboarding.md）：
//  1. 定义结构体，实现 Name() 与 Mount()。
//  2. 在 main.go 装配处用 registry.Register(cap) 注册。
//  3. Registry 在 Reconcile 时把工具/命令挂到共享 ToolRegistry/CommandRegistry，
//     并把这些挂载的 disposer 记录在案，Unload 时按插件维度回收。
type Capability interface {
	// Name 返回能力名（= 插件名，用于命名空间前缀与日志标签）。
	Name() string
	// Mount 把能力注册进共享的工具/命令注册表。返回的 Disposer 在
	// Unload/Reload 时按逆序调用以回收（注销工具、断开订阅等）。
	Mount(ctx *Capabilities) (Disposer, error)
}

// Disposer 释放一次 Mount 注册的全部副作用（幂等）。
type Disposer func()

// NopDisposer 是无副作用的 Disposer（能力未注册任何东西时返回）。
func NopDisposer() Disposer { return func() {} }

// Capabilities 是能力挂载时可用的宿主依赖集合。它把"扁平单例"收敛为
// 一个显式句柄：能力从它拿工具/命令注册表、事件总线，以及（可选）运行时
// 集成点。宿主在 Reconcile 时按序装配本实例并注入。
type Capabilities struct {
	Tools    *tools.ToolRegistry
	Commands *tools.CommandRegistry
	Events   *EventBus
	// Bridge is the live CoreBridge used by builtin capabilities that also
	// implement the public core.Plugin lifecycle.
	Bridge *CoreBridge
	// SetHooks installs the live hooks.json runtime on the Registry (nil = inert).
	// Injected by mountBuiltin; the hooks capability disposer calls SetHooks(nil).
	SetHooks func(*Hooks)
}

// 可选能力接口：宿主通过类型断言探测，非强制。

// LlmCapability 使内置能力能提供/替换 LLM 适配器（如自定义推理后端）。
type LlmCapability interface {
	LlmAdapter() llm.LlmAdapter
}

// SessionStoreCapability 使内置能力能提供/替换会话存储后端。
// 存储接口由 gateway.SessionStore 定义；此处以 any 承载以避免 plugin 包
// 反向依赖 gateway（保持插件边界单向：plugin → tools/llm）。
type SessionStoreCapability interface {
	SessionStore() any
}
