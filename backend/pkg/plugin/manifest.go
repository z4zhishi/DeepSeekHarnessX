package plugin

import (
	"encoding/json"
	"fmt"
)

// ABIVersion 是插件二进制协议（JSON-RPC 方法集 + manifest 结构）的版本。
// Host 只加载 ABI 兼容的插件；不匹配时报错并跳过（对齐 Cordis 的
// package.plugin 契约锁定，避免语义漂移）。
const ABIVersion = 1

// Manifest 声明一个插件的外部契约（对齐目标架构 §manifest.go）。
// 编译期内置能力也可声明 Manifest，但通常由 Registry 从其 Capability 接口
// 推导；磁盘上的外部插件必须携带 manifest 以便 Host 在启动子进程前校验。
type Manifest struct {
	// Name 是插件唯一名（限定命名空间：工具公开名以 <Name>__ 为前缀，
	// 事件 topic 以 <Name>.<topic> 为前缀）。
	Name string `json:"name"`
	// ABIVersion 须等于 ABIVersion，否则拒绝加载。
	ABIVersion int `json:"abiVersion"`
	// Capabilities 声明插件提供的能力（工具/命令）。
	Capabilities []CapabilitySpec `json:"capabilities"`
	// Deps 声明依赖的其它能力（如 "llm"、"session"）；Registry 按需解析，
	// 未满足则延迟到满足后再 Load（空时表示无依赖）。
	Deps []string `json:"deps,omitempty"`
	// Policy 声明权限与生命周期策略。
	Policy *Policy `json:"policy,omitempty"`
}

// Policy 描述插件的运行策略。
type Policy struct {
	// RequiresPerm 为 true 时，插件注册的工具默认走权限审批流水线。
	RequiresPerm *bool `json:"requiresPerm,omitempty"`
	// Reconnect 沿用 MCP 重连语义（断线自动重连并重同步）。
	Reconnect *Reconnect `json:"reconnect,omitempty"`
}

// Reconnect 描述重连策略（字段对齐 mcp.ReconnectConfig）。
type Reconnect struct {
	Enabled        *bool `json:"enabled,omitempty"`
	InitialDelayMs int   `json:"initialDelayMs,omitempty"`
	MaxDelayMs     int   `json:"maxDelayMs,omitempty"`
	MaxAttempts    int   `json:"maxAttempts,omitempty"`
}

// CapabilitySpec 描述一个能力的能力声明（目标架构 §CapabilitySpec）。
type CapabilitySpec struct {
	Name    string         `json:"name"`
	Methods []MethodSpec   `json:"methods"`
	Schema  map[string]any `json:"schema,omitempty"`
}

// MethodSpec 描述能力内一个方法（工具/命令）的契约。
type MethodSpec struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// Validate 校验 manifest 的结构正确性（不含运行时能力探测）。
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("plugin manifest: 为空")
	}
	if m.Name == "" {
		return fmt.Errorf("plugin manifest: 缺 name")
	}
	if m.ABIVersion != ABIVersion {
		return fmt.Errorf("plugin manifest %q: abiVersion 不匹配：got %d, want %d", m.Name, m.ABIVersion, ABIVersion)
	}
	for i := range m.Capabilities {
		if m.Capabilities[i].Name == "" {
			return fmt.Errorf("plugin manifest %q: 能力缺 name", m.Name)
		}
	}
	return nil
}

// ManifestError 描述 manifest 解析/校验错误。
type ManifestError struct{ msg string }

func (e *ManifestError) Error() string { return "plugin manifest: " + e.msg }
