package plugin

import (
	"log"
	"strings"
	"time"

	"dsh-go/pkg/llm"
	"dsh-go/pkg/mcp"
)

// HostCapabilityOptions is the production builtin-capability set used by dsh.
// Tests that want to assert owner maps must go through this helper rather than
// ad-hoc Register + RegisterBuiltinTools (that path remounts tools and wipes
// owners).
type HostCapabilityOptions struct {
	Adapter    llm.LlmAdapter
	Subagent   SubagentToolMount
	Hooks      HooksMountFunc
	MCPPath    string
	MCPTimeout time.Duration
	Logger     *log.Logger
	// ProtocolsEnabled 选择性关掉内置 protocols 能力的装配点。nil（默认）
	// 永远挂载：协议可用性是产品必选面，运行期下线走标准生命周期
	// SetEnabled("protocols", false)。该接缝仅为需要钉死固定能力集的测试保留。
	ProtocolsEnabled func() bool
}

// RegisterHostCapabilities mounts the builtin families the host actually
// ships. It does not register NewBuiltinToolsCapability: that catch-all
// re-runs RegisterBuiltinTools and deletes family owners.
func (r *Registry) RegisterHostCapabilities(opts HostCapabilityOptions) {
	if r == nil {
		return
	}
	if r.Libs != nil {
		r.Libs.Register(LibEntry{ID: "dshx-easy-api", Version: "1.0.0", Status: "supported"})
	}
	r.Register(FilesystemCapability())
	r.Register(PolicyCapability())
	r.Register(SessionQueryCapability())
	r.Register(TerminalCapability())
	r.Register(SkillCapability())
	r.Register(WorkflowCapability())
	r.Register(TaskCapability())
	r.Register(JobCapability())
	r.Register(GoalCapability())
	r.Register(ImageCapability())
	r.Register(TeamCapability())
	r.Register(CoreToolsCapability())
	r.Register(NewWebCapability())
	if opts.ProtocolsEnabled == nil || opts.ProtocolsEnabled() {
		// 线协议能力默认永远挂载：网关的适配器构造与协议清单经它的
		// 注册表解析，SetEnabled("protocols", false) 即下线全部协议。
		r.Register(NewProtocolsCapability())
	}
	if opts.Adapter != nil {
		r.Register(NewLlmProviderCapability(opts.Adapter))
	}
	if opts.Subagent != nil {
		r.Register(NewSubagentCapability(opts.Subagent))
	}
	if opts.Hooks != nil {
		r.Register(NewHooksCapability(opts.Hooks))
	}
	if strings.TrimSpace(opts.MCPPath) != "" {
		timeout := opts.MCPTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		r.Register(NewMcpCapability(opts.MCPPath, mcp.MountConfigFile, timeout, opts.Logger))
	}
}
