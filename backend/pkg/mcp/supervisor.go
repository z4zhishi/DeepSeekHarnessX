package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"dsh-go/pkg/tools"
)

// 上游契约（CK/packages/mcp/mcp-client/src/connection.ts，@b150a55）：
// - 连接主管：一次 outage 共享同一尝试预算（maxAttempts 次连续失败；
//   延迟从 initialDelayMs 起倍增到 maxDelayMs 封顶）
// - 连接稳定超过 maxDelayMs 视为前一次 outage 结束，下次断线重置预算
// - 连续失败耗尽预算 → 注销该服务器全部工具并停止（dispose 才能恢复）
// - notifications/tools/list_changed 通知 → 整代重同步
// - 日志前缀沿用上游格式 mcp(<serverName>):

// ReconnectConfig 对齐上游 reconnect 配置；零值字段取默认。
type ReconnectConfig struct {
	Enabled        *bool `json:"enabled,omitempty"`
	InitialDelayMs int   `json:"initialDelayMs,omitempty"`
	MaxDelayMs     int   `json:"maxDelayMs,omitempty"`
	MaxAttempts    int   `json:"maxAttempts,omitempty"`
}

const (
	reconnectDefaultInitialMs = 500
	reconnectDefaultMaxMs     = 30_000
	reconnectDefaultAttempts  = 10
)

func (c ReconnectConfig) enabled() bool { return c.Enabled == nil || *c.Enabled }

// resolved 补默认值（对齐上游 resolveReconnectPolicy 默认）。
func (c ReconnectConfig) resolved() ReconnectConfig {
	if c.InitialDelayMs <= 0 {
		c.InitialDelayMs = reconnectDefaultInitialMs
	}
	if c.MaxDelayMs <= 0 {
		c.MaxDelayMs = reconnectDefaultMaxMs
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = reconnectDefaultAttempts
	}
	return c
}

// BridgeOptions 对齐上游 ToolBridgeOptions。
type BridgeOptions struct {
	ServerName          string
	ToolCallTimeout     time.Duration
	RegistrationFailure string // "contain" | "throw"
	Logger              *log.Logger
}

// 命名空间保留：同一进程重复 serverName 是配置错误。
var (
	nsMu     sync.Mutex
	activeNS = map[string]bool{}
)

func reserveServerName(name string) error {
	nsMu.Lock()
	defer nsMu.Unlock()
	if activeNS[name] {
		return fmt.Errorf("mcp: serverName %q 已被其他连接占用", name)
	}
	activeNS[name] = true
	return nil
}

func releaseServerName(name string) {
	nsMu.Lock()
	defer nsMu.Unlock()
	delete(activeNS, name)
}

// connection 抽象一个已连通的 MCP 传输。
type connection interface {
	call(ctx context.Context, method string, params any, out any) error
	notifyServer(method string, params any)
	listTools(ctx context.Context, cursor string) (*ListToolsResult, error)
	callTool(ctx context.Context, rawName string, args map[string]any) (*CallToolResult, error)
	notifications() <-chan *rpcMsg
	// done 在远端断开/进程退出/流终止时触发（本地 Close 不触发）。
	done() <-chan struct{}
	close() error
}

// Supervisor 管理一个 MCP 服务器的连接生命周期。
type Supervisor struct {
	serverName    string
	reg           *tools.ToolRegistry
	opts          BridgeOptions
	reconnect     ReconnectConfig
	dial          func(ctx context.Context) (connection, error)
	label         string
	failOnStartup bool

	mu          sync.Mutex
	conn        connection
	disposers   map[string]func()
	notifyCh    <-chan *rpcMsg
	connSet     chan struct{} // 连接建立/更换时关闭，run 循环据此重读
	reconnects  int
	failed      int
	connectedAt time.Time
	closed      bool
	regErr      error

	stop chan struct{}
	done chan struct{}
}

// NewStdioSupervisor 创建 stdio 传输的 MCP 主管并完成首次连接/同步。
func NewStdioSupervisor(ctx context.Context, cfg StdioConfig, reg *tools.ToolRegistry, opts BridgeOptions) (*Supervisor, error) {
	validated, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	return newSupervisor(ctx, validated.ServerName, validated.FailOnStartupError, validated.Reconnect.resolved(), reg, opts, func(ctx context.Context) (connection, error) {
		return startStdio(ctx, validated)
	})
}

// NewHttpSupervisor 创建 streamable-http 传输的 MCP 主管。
func NewHttpSupervisor(ctx context.Context, cfg HttpConfig, reg *tools.ToolRegistry, opts BridgeOptions) (*Supervisor, error) {
	validated, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	return newSupervisor(ctx, validated.ServerName, validated.FailOnStartupError, validated.Reconnect.resolved(), reg, opts, func(ctx context.Context) (connection, error) {
		return startHTTPConn(ctx, validated)
	})
}

func newSupervisor(ctx context.Context, serverName string, failOnStartup bool, reconnect ReconnectConfig, reg *tools.ToolRegistry, opts BridgeOptions, dial func(context.Context) (connection, error)) (*Supervisor, error) {
	if err := reserveServerName(serverName); err != nil {
		return nil, err
	}
	if opts.ServerName == "" {
		opts.ServerName = serverName
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	s := &Supervisor{
		serverName:    serverName,
		reg:           reg,
		opts:          opts,
		reconnect:     reconnect,
		dial:          dial,
		label:         "mcp(" + serverName + ")",
		failOnStartup: failOnStartup,
		disposers:     map[string]func(){},
		connSet:       make(chan struct{}),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	if err := s.start(ctx); err != nil {
		releaseServerName(serverName)
		return nil, err
	}
	return s, nil
}

// start 执行首次连接；启动失败按 failOnStartupError 传播或转后台重连。
func (s *Supervisor) start(ctx context.Context) error {
	go s.run()
	if err := s.connectGeneration(ctx, true); err != nil {
		if s.failOnStartup {
			return err
		}
		s.logf("启动连接失败（将继续后台重连）：%v", err)
		s.scheduleReconnect(false)
	}
	return nil
}

// signalConnSet 通知 run() 连接代发生变化（幂等，然后重置通道）。
func (s *Supervisor) signalConnSet() {
	s.mu.Lock()
	if s.connSet != nil {
		close(s.connSet)
	}
	s.connSet = make(chan struct{})
	s.mu.Unlock()
}

// connectGeneration 尝试一次连接 + 初始同步。
func (s *Supervisor) connectGeneration(ctx context.Context, first bool) error {
	gen, err := s.dial(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.conn = gen
	s.notifyCh = gen.notifications()
	s.mu.Unlock()
	s.signalConnSet()

	defs, err := fetchGeneration(ctx, gen, bridgeOptions{
		serverName:          s.opts.ServerName,
		toolCallTimeout:     s.opts.ToolCallTimeout,
		registrationFailure: s.opts.RegistrationFailure,
	})
	if err != nil {
		s.generationDown(gen)
		return err
	}
	s.mu.Lock()
	s.disposers = s.swapGeneration(s.disposers, defs)
	s.connectedAt = time.Now()
	if !first {
		s.reconnects++
	}
	s.mu.Unlock()
	return nil
}

// generationDown 处理一代连接结束（断开/同步失败）：释放并调度重连。
func (s *Supervisor) generationDown(gen connection) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if gen != nil && s.conn != gen {
		s.mu.Unlock()
		return // 旧代，忽略
	}
	if gen != nil {
		s.conn = nil
		s.notifyCh = nil
	}
	s.mu.Unlock()
	s.signalConnSet()
	s.scheduleReconnect(gen != nil)
}

// scheduleReconnect 指数退避调度下一次连接尝试（对齐上游预算语义）。
func (s *Supervisor) scheduleReconnect(lostEstablished bool) {
	policy := s.reconnect
	if !policy.enabled() {
		s.logf("连接失败且重连已禁用——已注册工具将失效，直至进程重启")
		return
	}
	s.mu.Lock()
	// 稳定期（>= maxDelayMs）结束前一次 outage：重置预算
	if !s.connectedAt.IsZero() && time.Since(s.connectedAt) >= time.Duration(policy.MaxDelayMs)*time.Millisecond {
		s.failed = 0
	}
	s.connectedAt = time.Time{}
	s.failed++
	failed := s.failed
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	if failed > policy.MaxAttempts {
		s.mu.Lock()
		for _, dispose := range s.disposers {
			dispose()
		}
		s.disposers = map[string]func(){}
		s.mu.Unlock()
		s.logf("连续 %d 次重连失败，放弃——工具已注销；重启进程以恢复", policy.MaxAttempts)
		return
	}
	delay := time.Duration(policy.InitialDelayMs) * time.Millisecond
	for i := 1; i < failed && delay < time.Duration(policy.MaxDelayMs)*time.Millisecond; i++ {
		delay *= 2
	}
	if delay > time.Duration(policy.MaxDelayMs)*time.Millisecond {
		delay = time.Duration(policy.MaxDelayMs) * time.Millisecond
	}
	action := "连接失败，重试"
	if lostEstablished {
		action = "连接丢失，重连"
	}
	s.logf("%s：%dms 后第 %d/%d 次尝试", action, delay.Milliseconds(), failed, policy.MaxAttempts)
	select {
	case <-s.stop:
		return
	case <-time.After(delay):
	}
	s.reconnectNow()
}

// reconnectNow 立即发起一次重连尝试。
func (s *Supervisor) reconnectNow() {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.connectGeneration(ctx, false); err != nil {
		s.logf("重连失败：%v", err)
		s.scheduleReconnect(false)
		return
	}
	s.logf("重连成功，工具已重新同步")
}

// run 驱动通知监听与连接断开检测。
func (s *Supervisor) run() {
	defer close(s.done)
	for {
		s.mu.Lock()
		conn := s.conn
		closed := s.closed
		var notifyCh <-chan *rpcMsg
		if conn != nil {
			notifyCh = conn.notifications()
		}
		s.mu.Unlock()
		if closed {
			return
		}
		if conn == nil {
			select {
			case <-s.stop:
				return
			case <-s.connSet:
				continue
			}
		}
		select {
		case <-s.stop:
			return
		case <-conn.done():
			s.mu.Lock()
			current := s.conn == conn && !s.closed
			s.mu.Unlock()
			if current {
				s.logf("连接已断开")
				s.generationDown(conn)
			}
		case msg, ok := <-notifyCh:
			if !ok {
				continue
			}
			s.handleNotify(msg)
		}
	}
}

// handleNotify 处理服务器通知：tools/notifications 触发重同步。
func (s *Supervisor) handleNotify(msg *rpcMsg) {
	switch msg.Method {
	case "notifications/tools/list_changed":
		s.logf("工具列表已变化，重新同步")
		s.mu.Lock()
		conn := s.conn
		s.mu.Unlock()
		if conn == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.syncGeneration(ctx, conn); err != nil {
			s.logf("重同步失败：%v", err)
		}
	}
}

// syncGeneration 对指定连接执行一次全量同步（swap 串行化于 mu）。
func (s *Supervisor) syncGeneration(ctx context.Context, gen connection) error {
	defs, err := fetchGeneration(ctx, gen, bridgeOptions{
		serverName:          s.opts.ServerName,
		toolCallTimeout:     s.opts.ToolCallTimeout,
		registrationFailure: s.opts.RegistrationFailure,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disposers = s.swapGeneration(s.disposers, defs)
	return nil
}

// Resync 手动触发一次全量重同步（等价上游 notification 重同步路径）。
func (s *Supervisor) Resync(ctx context.Context) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return errors.New("mcp: 无活动连接")
	}
	return s.syncGeneration(ctx, conn)
}

// RegisteredTools 返回当前注册的公开工具名。
func (s *Supervisor) RegisteredTools() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.disposers))
	for name := range s.disposers {
		out = append(out, name)
	}
	return out
}

// Reconnects 返回累计成功重连次数（测试观测用）。
func (s *Supervisor) Reconnects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconnects
}

// Close 断开连接、注销全部工具、释放命名空间。
func (s *Supervisor) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conn := s.conn
	s.conn = nil
	s.notifyCh = nil
	for _, dispose := range s.disposers {
		dispose()
	}
	s.disposers = map[string]func(){}
	s.mu.Unlock()
	close(s.stop)
	s.signalConnSet()
	if conn != nil {
		_ = conn.close()
	}
	<-s.done
	releaseServerName(s.serverName)
	return nil
}

// swapGeneration 释放上一代并注册下一代；冲突整代回滚（该服务器零工具注册）。
// 必须持有 s.mu 调用。
func (s *Supervisor) swapGeneration(previous map[string]func(), next map[string]tools.ToolDefinition) map[string]func() {
	for _, dispose := range previous {
		dispose()
	}
	disposers := map[string]func(){}
	for name, def := range next {
		if err := s.reg.RegisterChecked(def); err != nil {
			for _, dispose := range disposers {
				dispose()
			}
			s.logf("工具注册失败（回滚本代，零工具注册）：%v", err)
			if s.opts.RegistrationFailure == "throw" {
				s.regErr = err
			}
			return map[string]func(){}
		}
		name := name
		disposers[name] = func() { s.reg.Unregister(name) }
	}
	return disposers
}

// logf 统一日志前缀。
func (s *Supervisor) logf(format string, args ...any) {
	s.opts.Logger.Printf(s.label+": "+format, args...)
}
