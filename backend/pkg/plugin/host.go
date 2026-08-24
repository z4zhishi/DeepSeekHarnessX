package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"dsh-go/pkg/mcp"
	"dsh-go/pkg/tools"
)

// hostConfig 描述一个外部子进程插件如何被拉起（复用 mcp.StdioConfig 传输）。
type hostConfig struct {
	// Name 是插件名（须与 manifest 一致，用于命名空间前缀与日志标签）。
	Name string
	// Command 是要启动的可执行文件。
	Command string
	// Args 是子进程参数。
	Args []string
	// Env 追加子进程环境变量。
	Env map[string]string
	// Cwd 子进程工作目录。
	Cwd string
	// RequiresPerm 控制插件注册工具是否默认走权限审批流水线（nil 默认 true）。
	RequiresPerm *bool
	// Reconnect 重连策略（沿用 mcp 语义；零值取默认）。
	Reconnect mcp.ReconnectConfig
	// Logger 日志输出（nil 时内部用 log.Default）。接受任何具备 Printf(string, ...any)
	// 的类型（*log.Logger、注册表日志适配器等）。
	Logger interface{ Printf(string, ...any) }
}

// resolveReconnect 补默认重连参数（对齐 mcp.ReconnectConfig.resolved）。
func resolveReconnect(c mcp.ReconnectConfig) mcp.ReconnectConfig {
	if c.InitialDelayMs <= 0 {
		c.InitialDelayMs = 500
	}
	if c.MaxDelayMs <= 0 {
		c.MaxDelayMs = 30_000
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 10
	}
	return c
}

// Host 管理一个外部 JSON-RPC 插件子进程的生命周期与工具/命令/事件挂载。
//
// 握手协议（宿主视角）：
//
//	Host → 子进程  capability/initialize → 子进程返回 {protocolVersion, serverName}
//	Host → 子进程  capability/list       → 子进程返回能力声明（CapabilitySpec[]）
//	Host → 子进程  tool/register         → 子进程返回该方法的 ToolDefinition
//	Host → 子进程  command/register      → 子进程返回命令定义
//	Host → 子进程  event/subscribe       → 声明宿主转发的 topic 集
//	工具调用      tool/call              → 子进程执行并返回结果
//	命令执行      command/execute        → 子进程执行并返回结果
//	子进程 → Host  notifications/event/* → 转投 EventBus（事件发布）
//
// 断线自动重连（复用 mcp stdio 传输 + 指数退避），重连后整代重同步。生命周期
// 由 Host.Close 统一管理（close 时 kill 子进程）。
type Host struct {
	conf   hostConfig
	reg    *tools.ToolRegistry
	cmds   *tools.CommandRegistry
	bus    *EventBus
	label  string
	log    interface{ Printf(string, ...any) }
	dial   func(ctx context.Context) (mcp.Transport, error)
	reconn mcp.ReconnectConfig

	mu          sync.Mutex
	transport   mcp.Transport
	disposers   []Disposer
	connectedAt time.Time
	failed      int
	closed      bool

	connSet chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

// NewHost 构造并连接一个外部插件 Host（首次握手 + 同步）。连接失败不阻断
// 构造：Host 转入后台指数退避重连（对齐 MCP Supervisor 语义）。
func NewHost(ctx context.Context, cfg hostConfig, reg *tools.ToolRegistry, cmds *tools.CommandRegistry, bus *EventBus) (*Host, error) {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	h := &Host{
		conf:    cfg,
		reg:     reg,
		cmds:    cmds,
		bus:     bus,
		label:   cfg.Name,
		log:     cfg.Logger,
		reconn:  resolveReconnect(cfg.Reconnect),
		connSet: make(chan struct{}),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	stdio := mcp.StdioConfig{
		ServerName: cfg.Name,
		Command:    cfg.Command,
		Args:       cfg.Args,
		Env:        cfg.Env,
		Cwd:        cfg.Cwd,
	}
	h.dial = func(ctx context.Context) (mcp.Transport, error) {
		return mcp.StartStdioTransport(stdio)
	}
	go h.run()
	if err := h.connectGeneration(ctx, true); err != nil {
		h.logf("启动连接失败（将继续后台重连）：%v", err)
		h.scheduleReconnect(false)
	}
	return h, nil
}

func (h *Host) logf(format string, args ...any) {
	h.log.Printf("plugin(%s): "+format, append([]any{h.label}, args...)...)
}

// pluginCapabilityResult 是 tool/register 与 command/register 的返回体。
type pluginCapabilityResult struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// connectGeneration 连接子进程并整代同步工具/命令。
func (h *Host) connectGeneration(ctx context.Context, first bool) error {
	tr, err := h.dial(ctx)
	if err != nil {
		return err
	}
	// 握手：capability/initialize → 返回插件 ABI 版本与自报名。
	var initRes struct {
		ProtocolVersion int    `json:"protocolVersion"`
		ServerName      string `json:"serverName"`
	}
	if err := tr.Call(ctx, "capability/initialize", map[string]any{
		"clientName": "dsh-go-plugin-host",
		"clientVer":  ABIVersion,
	}, &initRes); err != nil {
		_ = tr.Close()
		return fmt.Errorf("capability/initialize: %w", err)
	}
	if initRes.ProtocolVersion != ABIVersion {
		_ = tr.Close()
		return fmt.Errorf("插件 ABI 版本 %d 不匹配（want %d）", initRes.ProtocolVersion, ABIVersion)
	}
	if initRes.ServerName != "" && initRes.ServerName != h.conf.Name {
		h.logf("警告：插件自报名 %q 与配置 %q 不一致", initRes.ServerName, h.conf.Name)
	}

	disposers, err := h.syncGeneration(ctx, tr)
	if err != nil {
		_ = tr.Close()
		return err
	}

	h.mu.Lock()
	h.transport = tr
	h.disposers = disposers
	h.connectedAt = time.Now()
	h.mu.Unlock()
	h.signalConnSet()
	return nil
}

// syncGeneration 拉取能力清单并整代注册到共享注册表。
func (h *Host) syncGeneration(ctx context.Context, tr mcp.Transport) ([]Disposer, error) {
	var caps struct {
		Capabilities []CapabilitySpec `json:"capabilities"`
	}
	if err := tr.Call(ctx, "capability/list", map[string]any{}, &caps); err != nil {
		return nil, fmt.Errorf("capability/list: %w", err)
	}
	prefix := h.conf.Name + "__"
	var disposers []Disposer
	for _, cap := range caps.Capabilities {
		for _, m := range cap.Methods {
			switch cap.Name {
			case "tool", "tools":
				if err := h.registerTool(ctx, tr, prefix, m, &disposers); err != nil {
					return nil, err
				}
			case "command", "commands":
				if err := h.registerCommand(ctx, tr, m); err != nil {
					return nil, err
				}
			}
		}
	}
	// event/subscribe：声明接收插件的全部事件通知。
	_ = tr.Call(ctx, "event/subscribe", map[string]any{"topics": []string{"*"}}, nil)
	return disposers, nil
}

func (h *Host) registerTool(ctx context.Context, tr mcp.Transport, prefix string, m MethodSpec, disposers *[]Disposer) error {
	var def pluginCapabilityResult
	if err := tr.Call(ctx, "tool/register", map[string]any{"name": m.Name}, &def); err != nil {
		return fmt.Errorf("tool/register %q: %w", m.Name, err)
	}
	if def.Name == "" {
		def.Name = m.Name
	}
	params := def.Parameters
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object"}`)
	}
	desc := def.Description
	if desc == "" {
		desc = m.Description
	}
	requiresPerm := true
	if h.conf.RequiresPerm != nil {
		requiresPerm = *h.conf.RequiresPerm
	}
	rawName := def.Name
	pub := h.conf.Name + "__" + rawName
	if err := h.reg.RegisterChecked(tools.ToolDefinition{
		Name:           pub,
		Description:    desc,
		ParametersJSON: params,
		RequiresPerm:   requiresPerm,
		Execute: func(ctx tools.ToolExecutionContext, argsJSON string) (any, error) {
			return h.callTool(ctx, tr, rawName, argsJSON)
		},
	}); err != nil {
		return fmt.Errorf("tool 注册冲突 %q: %w", pub, err)
	}
	*disposers = append(*disposers, func() { h.reg.Unregister(pub) })
	return nil
}

// callTool 通过 tool/call 调用远端工具并解析为规范化结果。
func (h *Host) callTool(ctx tools.ToolExecutionContext, tr mcp.Transport, rawName string, argsJSON string) (any, error) {
	var arguments map[string]any
	_ = json.Unmarshal([]byte(argsJSON), &arguments)
	if arguments == nil {
		arguments = map[string]any{}
	}
	var res mcp.CallToolResult
	if err := tr.Call(ctx.Context, "tool/call", map[string]any{
		"name":      rawName,
		"arguments": arguments,
	}, &res); err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, errors.New(mcp.ExtractText(res.Content))
	}
	return mcp.McpResult{Content: res.Content}, nil
}

func (h *Host) registerCommand(ctx context.Context, tr mcp.Transport, m MethodSpec) error {
	var def pluginCapabilityResult
	if err := tr.Call(ctx, "command/register", map[string]any{"name": m.Name}, &def); err != nil {
		return fmt.Errorf("command/register %q: %w", m.Name, err)
	}
	if def.Name == "" {
		def.Name = m.Name
	}
	rawName := def.Name
	cmdName := def.Name
	desc := def.Description
	if desc == "" {
		desc = m.Description
	}
	h.cmds.Register(tools.CommandDefinition{
		Name:        cmdName,
		Description: desc,
		Handler: func(inv tools.CommandInvocation) tools.CommandResult {
			var res struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			}
			if err := tr.Call(context.Background(), "command/execute", map[string]any{
				"name":    rawName,
				"rawArgs": inv.RawInput,
			}, &res); err != nil {
				return tools.CommandResult{Kind: "error", Text: fmt.Sprintf("/%s: %v", cmdName, err)}
			}
			if res.Kind == "" {
				res.Kind = "success"
			}
			return tools.CommandResult{Kind: res.Kind, Text: res.Text}
		},
	})
	return nil
}

// signalConnSet 通知 run() 连接代变化（幂等，然后重置通道）。
func (h *Host) signalConnSet() {
	h.mu.Lock()
	if h.connSet != nil {
		close(h.connSet)
	}
	h.connSet = make(chan struct{})
	h.mu.Unlock()
}

// run 监听连接断开并调度重连。
func (h *Host) run() {
	defer close(h.done)
	for {
		h.mu.Lock()
		tr := h.transport
		closed := h.closed
		h.mu.Unlock()
		if closed {
			return
		}
		if tr == nil {
			// 快照 connSet 到局部变量再 select：字段仅能在 mu 下读写，避免
			// signalConnSet 锁内重建字段与 run 无锁读字段的竞态。
			h.mu.Lock()
			connSet := h.connSet
			h.mu.Unlock()
			select {
			case <-h.stop:
				return
			case <-connSet:
				continue
			}
		}
		select {
		case <-h.stop:
			return
		case <-tr.Done():
			h.mu.Lock()
			current := h.transport == tr && !h.closed
			h.mu.Unlock()
			if current {
				h.logf("连接已断开，注销当前代工具")
				h.teardownGeneration()
				h.scheduleReconnect(true)
			}
		case msg := <-tr.Notifications():
			h.handleNotify(msg)
		}
	}
}

// teardownGeneration 逆序释放当前代全部 disposer 并清空 transport。
func (h *Host) teardownGeneration() {
	h.mu.Lock()
	for i := len(h.disposers) - 1; i >= 0; i-- {
		if h.disposers[i] != nil {
			h.disposers[i]()
		}
	}
	h.disposers = nil
	h.transport = nil
	h.mu.Unlock()
	h.signalConnSet()
}

// handleNotify 把子进程通知发布到 EventBus（topic: <pluginName>.<sub>）。
func (h *Host) handleNotify(msg *mcp.RPC) {
	if msg == nil || msg.ID != nil {
		return
	}
	sub := strings.TrimPrefix(msg.Method, "notifications/event/")
	if sub == msg.Method {
		sub = strings.TrimPrefix(msg.Method, "notifications/")
	}
	topic := h.conf.Name + "." + sub
	var payload any
	if len(msg.Params) > 0 {
		_ = json.Unmarshal(msg.Params, &payload)
	}
	h.bus.Emit(topic, payload)
}

// scheduleReconnect 指数退避调度重连。
func (h *Host) scheduleReconnect(_ bool) {
	policy := h.reconn
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.failed++
	failed := h.failed
	h.mu.Unlock()
	if failed > policy.MaxAttempts {
		h.logf("连续 %d 次重连失败，放弃", policy.MaxAttempts)
		return
	}
	delay := time.Duration(policy.InitialDelayMs) * time.Millisecond
	for i := 1; i < failed && delay < time.Duration(policy.MaxDelayMs)*time.Millisecond; i++ {
		delay *= 2
	}
	if delay > time.Duration(policy.MaxDelayMs)*time.Millisecond {
		delay = time.Duration(policy.MaxDelayMs) * time.Millisecond
	}
	h.logf("重连：%dms 后第 %d/%d 次尝试", delay.Milliseconds(), failed, policy.MaxAttempts)
	select {
	case <-h.stop:
		return
	case <-time.After(delay):
	}
	h.reconnectNow()
}

func (h *Host) reconnectNow() {
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.connectGeneration(ctx, false); err != nil {
		h.logf("重连失败：%v", err)
		h.scheduleReconnect(false)
	}
}

// RegisteredTools 返回当前挂载的公开工具名（前缀匹配；测试观测用）。
func (h *Host) RegisteredTools() []string {
	prefix := h.conf.Name + "__"
	decls := h.reg.ListDeclarations()
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		if strings.HasPrefix(d.Name, prefix) {
			out = append(out, d.Name)
		}
	}
	return out
}

// Close 关闭连接、注销全部工具、停止重连。
func (h *Host) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	tr := h.transport
	h.transport = nil
	for i := len(h.disposers) - 1; i >= 0; i-- {
		if h.disposers[i] != nil {
			h.disposers[i]()
		}
	}
	h.disposers = nil
	h.mu.Unlock()
	close(h.stop)
	h.signalConnSet()
	if tr != nil {
		_ = tr.Close()
	}
	<-h.done
	return nil
}
