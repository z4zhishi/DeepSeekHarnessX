package plugin

import (
	"errors"

	"dsh-go/pkg/llm"
	"dsh-go/pkg/tools"
)

// errSubagentUnavailable 是子代理能力挂载时宿主依赖缺失的错误。
var errSubagentUnavailable = errors.New("plugin(subagent): tool registry or mount unavailable")

// LlmProviderCapability 把 LLM 适配器的提供收敛为插件能力（Phase 2 迁移
// 第 5 项）。Mount 时不注册任何工具，只通过 LlmCapability 可选接口向宿主
// 声明适配器；宿主（main 装配处）从 registry.LlmAdapter() 读取。
// Disposer 幂等：LLM 是宿主级单例，由 Registry.Unload 清空引用完成回收。
type llmProviderCapability struct {
	adapter llm.LlmAdapter
}

// NewLlmProviderCapability 构造一个由插件托管的 LLM provider 能力。
func NewLlmProviderCapability(adapter llm.LlmAdapter) Capability {
	return llmProviderCapability{adapter: adapter}
}

func (c llmProviderCapability) Name() string { return "llm-provider" }

func (c llmProviderCapability) Mount(_ *Capabilities) (Disposer, error) {
	return func() {}, nil
}

func (c llmProviderCapability) LlmAdapter() llm.LlmAdapter {
	return c.adapter
}

var _ Capability = llmProviderCapability{}
var _ LlmCapability = llmProviderCapability{}

// SubagentToolMount 是宿主注入的子代理工具注册回调（plugin 不反向依赖
// subagent 包，保持边界单向）。
type SubagentToolMount func(r *tools.ToolRegistry)

// subagentCapability 包装宿主注入的子代理工具注册，使其成为带 owner 的
// 能力；卸载时注销全部由它注册的工具（invoke_subagent / list_subagents /
// list_descendants）。
type subagentCapability struct {
	mount SubagentToolMount
}

// NewSubagentCapability 构造子代理调度能力。mount 由宿主提供（通常是
// subagent.Manager.RegisterSubagentTools 的绑定）。
func NewSubagentCapability(mount SubagentToolMount) Capability {
	return subagentCapability{mount: mount}
}

func (c subagentCapability) Name() string { return "subagent" }

func (c subagentCapability) Mount(cap *Capabilities) (Disposer, error) {
	if cap == nil || cap.Tools == nil || c.mount == nil {
		return nil, errSubagentUnavailable
	}
	before := cap.Tools.Names()
	c.mount(cap.Tools)
	after := cap.Tools.Names()
	beforeSet := make(map[string]bool, len(before))
	for _, name := range before {
		beforeSet[name] = true
	}
	owned := make([]string, 0, len(after))
	for _, name := range after {
		if !beforeSet[name] {
			owned = append(owned, name)
		}
	}
	cap.Tools.ClaimOwner(c.Name(), owned...)
	return func() {
		for _, name := range owned {
			cap.Tools.UnregisterOwned(c.Name(), name)
		}
	}, nil
}

var _ Capability = subagentCapability{}
