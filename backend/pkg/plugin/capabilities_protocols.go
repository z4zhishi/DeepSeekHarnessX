package plugin

import (
	"dsh-go/pkg/llm"
)

// 协议支持也是插件平台的一等能力（用户指令：将协议支持也插件化，并作内部
// 协议以实现中转，移除 deepseek 协议——deepseek 没有自有协议，还是老三样）。
//
// 架构分层：
//   - 内部协议（llm.InternalProtocol = dsh-internal/v1）即 LlmAdapter 接口本身，
//     它是中转（relay）的协议表示，不注册进任何线协议注册表；
//   - 三条线协议（openai-completions / openai-responses / anthropic-messages）
//     是注册项，由本内置 protocols 能力在 Mount 时登记进 自己持有的
//     *llm.ProtocolRegistry，Disposer 注销；
//   - SetEnabled("protocols", false) 走标准 Registry 生命周期（Unload →
//     disposer 注销登记项），网关的适配器构造与协议清单经
//     Registry.ProtocolRegistry() 读取本能力发布的注册表，disable 即刻
//     收窄为空表（Build 报 unknown protocol），不会再静默走包默认注册表；
//   - DeepSeek 不是协议：provider "deepseek" 是 openai-completions 之上的
//     默认 provider profile（api.deepseek.com、凭据 DEEPSEEK_API_KEY），
//     协议旧值归一化在网关设置读取处（normalizeStoredProtocol）。

// ProtocolCapabilityName 是内置协议能力的固定能力名（注册表查找键）。
const ProtocolCapabilityName = "protocols"

// ProtocolRegistry 是网关消费的线协议注册表只读面（List / Build）。实例是
// pkg/llm 的 *ProtocolRegistry；登记/注销的生命周期属于本能力，不外露 ——
// 能力 Mount 登记 / Disposer 注销是协议可用性的唯一变更路径。
type ProtocolRegistry interface {
	// List 返回当前已登记协议名（登记序；首位 openai-completions 为缺省）。
	List() []string
	// Build 按协议名构造适配器；未登记协议返回
	// "llm: unknown protocol %q (supported: …)" —— 与包默认构造的错误同形。
	Build(name string, profile llm.ProviderProfile) (llm.LlmAdapter, error)
}

var _ ProtocolRegistry = (*llm.ProtocolRegistry)(nil)

// ProtocolsCapability 可选能力接口：能力通过它向宿主发布线协议注册表。
type ProtocolsCapability interface {
	Protocols() ProtocolRegistry
}

// protocolsCapability 是内置线协议能力。注册表实例与能力实例同生共死；
// Mount 登记 / Disposer 注销，Reload 重挂复用同一实例（W2-a：remount 复用
// 同一不可变 Capability 实例），因此宿主句柄 Registry.ProtocolRegistry()
// 的指针身份在 disable/enable 循环间稳定。
type protocolsCapability struct {
	reg *llm.ProtocolRegistry
}

// NewProtocolsCapability 构造内置协议能力：空注册表（llm.NewProtocolRegistry），
// 由 Mount 经 llm.RegisterBuiltinProtocols 播种三条线协议（登记序 = SupportedProtocols
// 稳定序）。不使用 llm 包级 defaultProtocols：包默认表是免插件路径的回退，
// 能力持有独立实例，enable/disable 才有可观测效果。
func NewProtocolsCapability() Capability {
	return &protocolsCapability{reg: llm.NewProtocolRegistry()}
}

func (c *protocolsCapability) Name() string { return ProtocolCapabilityName }

func (c *protocolsCapability) Mount(_ *Capabilities) (Disposer, error) {
	// 播种与 llm 包默认注册表同一条线协议集（同工厂、同顺序）——线实现
	// （线格式、鉴权头、SSE 解析）仍在 pkg/llm，能力只管理注册表生命周期。
	if err := llm.RegisterBuiltinProtocols(c.reg); err != nil {
		return nil, err
	}
	names := llm.SupportedProtocols()
	return func() {
		// 注销按登记序全量回收；未登记项的显式失败错误在此无通道上报，
		// 忽略即可（Unregister 只对"已登记名"失败，播种序与登记序一致）。
		for _, name := range names {
			_ = c.reg.Unregister(name)
		}
	}, nil
}

func (c *protocolsCapability) Protocols() ProtocolRegistry { return c.reg }

var (
	_ Capability          = (*protocolsCapability)(nil)
	_ ProtocolsCapability = (*protocolsCapability)(nil)
)
