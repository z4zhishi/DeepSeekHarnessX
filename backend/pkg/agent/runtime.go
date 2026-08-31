package agent

import (
	"dsh-go/pkg/llm"
	"dsh-go/pkg/plugin"
)

// PluginRuntime is the live plugin host (*plugin.Registry).
// Agents re-read it on every hook dispatch and every LLM Stream.
type PluginRuntime interface {
	Hooks() *plugin.Hooks
	LlmAdapter() llm.LlmAdapter
	HasBuiltin(name string) bool
}

// AttachPluginRuntime binds a live plugin host to this agent. Product spawn
// paths call this after NewAgent so Unload/SetEnabled are visible without
// changing NewAgent's signature. Nil-safe.
func (a *Agent) AttachPluginRuntime(rt PluginRuntime) {
	if a == nil {
		return
	}
	a.pluginRuntime = rt
	if rt != nil {
		a.HooksProvider = rt
		// Optional: hosts whose runtime is *plugin.Registry expose EventBus.
		// PluginCtl (gateway) does not have to; spawnAgent already sets HookBus.
		if hp, ok := rt.(interface{ EventBus() *plugin.EventBus }); ok {
			if bus := hp.EventBus(); bus != nil {
				a.HookBus = bus
			}
		}
	}
}

// liveAdapter returns the adapter used for this Stream/title call.
// When llm-provider is a registered builtin, disable/unload is visible as
// a nil live adapter (honest fail, no panic). While the builtin still
// exposes a non-nil adapter, the constructor pointer is kept so host
// wrappers (TUI tunedAdapter for effort/usage) are not bypassed.
//
// 热换限制 (W2-a, pinned by TestLiveAdapterRemountAdapterInstance):
// llm-provider 不支持运行期热换 adapter 实例。SetEnabled(enabled=true)/Reload
// 的 remount 走 mountBuiltin(r.builtins[name])，重挂的是同一个不可变
// Capability，Registry.LlmAdapter() 指回同一实例；运行期替换内容走
// llm.Router.Swap（同实例换 delegate）。若宿主绕开这两条路径换装新实例
// （例如重新 RegisterHostCapabilities），已 spawn 的 agent 与 TUI
// tunedAdapter 继续持有旧实例（本函数仍返回构造时包装器），新实例只对
// 之后新建的 agent 可见；换实例 = 重启进程。
func (a *Agent) liveAdapter() llm.LlmAdapter {
	if a == nil {
		return nil
	}
	if a.pluginRuntime != nil && a.pluginRuntime.HasBuiltin("llm-provider") {
		if a.pluginRuntime.LlmAdapter() == nil {
			return nil
		}
		if a.LlmAdapter != nil {
			return a.LlmAdapter
		}
		return a.pluginRuntime.LlmAdapter()
	}
	return a.LlmAdapter
}
