// Package acp implements the agent-side Agent Client Protocol (ACP) stdio
// bridge in the standard wire format produced by @agentclientprotocol/sdk:
// newline-delimited JSON-RPC 2.0, initialize / session/new / session/prompt /
// session/cancel, session/update progress notifications discriminated by the
// `sessionUpdate` field, and server->client requestPermission request/response.
//
// The bridge exposes fresh DeepSeekHarnessX (DSHX) harness sessions to trusted
// programmatic clients (Zed, VS Code, any standard ACP client): prompt text
// blocks, committed assistant text, cancellation, and one-shot permission
// decisions. Contract ported from the upstream reference implementation;
// product naming is DeepSeekHarnessX/DSHX only.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dsh-go/pkg/agent"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/mcp"
	"dsh-go/pkg/session"
	"dsh-go/pkg/tools"
)

// ProtocolVersion is the single ACP protocol revision this agent speaks
// (upstream PROTOCOL_VERSION). The spec's "same version if supported, else the
// latest supported" both resolve to this server's one version.
const ProtocolVersion = 1

// Agent identity advertised in InitializeResponse.agentInfo.
const (
	AgentName    = "deepseekharnessx-acp"
	AgentTitle   = "DeepSeekHarnessX ACP"
	AgentVersion = "0.1.0"
)

// StopReason is the ACP terminal reason vocabulary (upstream codec.ts).
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopMaxTokens StopReason = "max_tokens"
	StopCancelled StopReason = "cancelled"
	StopRefusal   StopReason = "refusal"
)

// requestPermission option ids carrying allow_once/reject_once semantics
// (upstream PermissionOptionKind values).
const (
	optAllowOnce  = "allow-once"
	optRejectOnce = "reject-once"

	outcomeSelected = "selected"
)

// JSON-RPC 2.0 / ACP RequestError codes used on the wire.
const (
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternalError  = -32603
)

// permissionTimeout bounds one server->client requestPermission round trip;
// a missing or timed-out answer cancels the tool call. Overridable in tests.
var permissionTimeout = 60 * time.Second

// session/new 内联 mcpServers 透传限制（生态收编：标准 {名称: {定义}} 配置
// 形状，经 mcp.ImportConfig 翻译后挂载；载荷/服务器数双重封顶）。
const (
	maxInlineMCPBytes   = 64 << 10
	maxInlineMCPServers = 16
)

// ContentBlock mirrors one ACP prompt content block (upstream content.ts).
// Only type:"text" is admitted onto the model surface; every other family
// fails fast with invalidParams instead of being silently dropped.
type ContentBlock struct {
	Type     string `json:"type"` // "text" | "image" | "audio" | "resource_link" | "resource"
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Name     string `json:"name,omitempty"` // resource_link display name
	URI      string `json:"uri,omitempty"`  // resource_link target
}

// rpcFrame is one inbound JSON-RPC 2.0 frame. Requests carry method+params;
// responses to our requests carry id+result or id+error and no method.
type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// rpcError is the JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// hasID reports whether the frame carries a non-null JSON-RPC id.
func (f *rpcFrame) hasID() bool {
	return len(f.ID) > 0 && string(f.ID) != "null"
}

// unquoteID decodes a raw JSON id into its scalar value: a quoted string id
// loses its quotes, numeric ids keep their literal text. Empty on malformed.
func unquoteID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	return string(raw)
}

// acpRequestError marks a handler failure that already carries its wire code
// (upstream RequestError.invalidParams / internalError).
type acpRequestError struct {
	code int
	msg  string
}

func (e *acpRequestError) Error() string { return e.msg }

func invalidParams(format string, args ...any) error {
	return &acpRequestError{code: errInvalidParams, msg: fmt.Sprintf(format, args...)}
}

func internalError(format string, args ...any) error {
	return &acpRequestError{code: errInternalError, msg: fmt.Sprintf(format, args...)}
}

func newACPRequestError(code int, format string, args ...any) *acpRequestError {
	return &acpRequestError{code: code, msg: fmt.Sprintf(format, args...)}
}

// isEmptyParams reports whether params is absent or null.
func isEmptyParams(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// promptInflight tracks one whole-turn settlement for session/prompt. The RPC
// response resolves only after the prompted turn has gone silent: its exact
// turn/end landed after every assistant delivery was written. The prompt is
// correlated to its own turn by the unique user/message id — the relay sees the
// strictly ordered envelope stream, so message identity is authoritative where
// harness turn numbering cannot be relied on across producers.
type promptInflight struct {
	msgID      string
	settled    chan struct{}
	closeOnce  sync.Once
	stopReason StopReason // written before settled closes, read after
	mu         sync.Mutex
	bound      bool // the relay saw this exact message enter its turn
}

// bind marks that the inflight prompt's own message entered its turn; reports
// false when already bound.
func (p *promptInflight) bind() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bound {
		return false
	}
	p.bound = true
	return true
}

func (p *promptInflight) isBound() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bound
}

// settle resolves the prompt exactly once with reason; later callers lose,
// so an explicit client cancel wins over any concurrent natural settlement.
func (p *promptInflight) settle(reason StopReason) {
	p.closeOnce.Do(func() {
		p.stopReason = reason
		close(p.settled)
	})
}

// SessionRecord is per-session protocol state: one live actor plus the single
// in-flight prompt slot reserved before admission (upstream SessionRecord).
type SessionRecord struct {
	agent *agent.Agent

	mu       sync.Mutex
	inflight *promptInflight

	// stopRelay ends this session's event-relay goroutine only; the actor
	// itself keeps running until process shutdown.
	stopRelay context.CancelFunc
}

// Server implements the ACP stdio server in the standard wire format.
type Server struct {
	reader  *bufio.Reader
	writer  io.Writer
	writeMu sync.Mutex

	tools      *tools.ToolRegistry
	llmAdapter llm.LlmAdapter
	plugins    agent.PluginRuntime

	// mountInlineMCP 把 session/new 的内联 mcpServers（已翻译的 FileConfig）
	// 挂载进注册表；测试可注入替换。生产实现走 mcp.MountConfig。
	mountInlineMCP func(ctx context.Context, cfg *mcp.FileConfig, reg *tools.ToolRegistry, logger *log.Logger) ([]*mcp.Supervisor, error)

	// mcpMu 串行化内联 MCP 挂载（注册表与 serverName 命名空间为进程级共享）；
	// mcpSups 持有连接生命周期的透传 Supervisor（会话与代理同为连接生命周期，
	// 进程关闭时统一回收）。
	mcpMu   sync.Mutex
	mcpSups []*mcp.Supervisor

	mu       sync.Mutex
	sessions map[string]*SessionRecord

	// pendingPermissions maps a server-issued requestPermission frame id to
	// the channel that the client's JSON-RPC response wakes. The channel wake
	// mechanism is internal wiring reused from the previous approval
	// waterfall; on the wire the direction is strictly
	// server request -> client response.
	pendingPermissions map[string]chan tools.ApprovalDecision
	nextPermissionID   int
	closed             bool
}

// NewServer creates a new ACP stdio server over stdin/stdout.
func NewServer(toolReg *tools.ToolRegistry, adapter llm.LlmAdapter) *Server {
	return NewServerWithIO(os.Stdin, os.Stdout, toolReg, adapter)
}

// NewServerWithIO builds a server over injected streams (tests).
func NewServerWithIO(r io.Reader, w io.Writer, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter) *Server {
	return &Server{
		reader:             bufio.NewReader(r),
		writer:             w,
		tools:              toolReg,
		llmAdapter:         adapter,
		mountInlineMCP:     mountInlineMCPDefault,
		sessions:           make(map[string]*SessionRecord),
		pendingPermissions: make(map[string]chan tools.ApprovalDecision),
	}
}

// AttachPluginRuntime binds the live plugin host so ACP-created agents
// re-read hooks/llm-provider on each dispatch/Stream. Nil-safe.
func (s *Server) AttachPluginRuntime(rt agent.PluginRuntime) {
	if s == nil {
		return
	}
	s.plugins = rt
}

// Serve reads newline-delimited JSON-RPC frames until EOF or ctx cancellation.
// Each request is dispatched on its own goroutine so a long turn never blocks
// the reader: session/cancel must stay servable while session/prompt waits.
func (s *Server) Serve(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			line, err := s.reader.ReadBytes('\n')
			if err != nil {
				if err != io.EOF {
					s.mu.Lock()
					s.closed = true
					s.mu.Unlock()
				}
				return
			}
			s.handleLine(line)
		}
	}()
	select {
	case <-ctx.Done():
		return nil
	case <-done:
		return nil
	}
}

// handleLine parses and routes exactly one inbound frame. Malformed lines are
// dropped silently (upstream ndJsonStream behavior).
func (s *Server) handleLine(line []byte) {
	var frame rpcFrame
	if err := json.Unmarshal(line, &frame); err != nil {
		return // malformed: dropped like upstream
	}
	if frame.Method == "" {
		// Response to one of our requests: the requestPermission answer.
		if frame.hasID() {
			s.resolvePermission(unquoteID(frame.ID), frame.Result, frame.Error)
		}
		return
	}
	if frame.hasID() {
		// Request: dispatched on its own goroutine so a long turn never
		// blocks the reader loop.
		go s.handleRequest(frame)
		return
	}
	// Notification: no response obligation, but it MUST be acted upon.
	switch frame.Method {
	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(frame.Params, &p); err == nil && p.SessionID != "" {
			go s.cancel(p.SessionID)
		}
	default:
		// Unknown notifications are ignored like upstream.
	}
}

// handleRequest dispatches one ACP request and writes its response frame.
func (s *Server) handleRequest(frame rpcFrame) {
	result, rpcErr := s.dispatch(frame.Method, frame.Params)
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(frame.ID),
	}
	if rpcErr != nil {
		code := errInternalError
		if re, ok := rpcErr.(*acpRequestError); ok {
			code = re.code
		}
		resp["error"] = map[string]any{"code": code, "message": rpcErr.Error()}
	} else {
		resp["result"] = result // nil marshals as null, matching upstream void results
	}
	s.write(resp)
}

// dispatch routes one standard ACP method.
func (s *Server) dispatch(method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return s.initialize()

	case "session/new":
		var p struct {
			Cwd                   string          `json:"cwd"`
			McpServers            json.RawMessage `json:"mcpServers"`
			AdditionalDirectories []string        `json:"additionalDirectories"`
		}
		if err := json.Unmarshal(params, &p); err != nil && !isEmptyParams(params) {
			return nil, invalidParams("session/new params: %v", err)
		}
		return s.newSession(p.Cwd, p.AdditionalDirectories, p.McpServers)

	case "session/prompt":
		var p struct {
			SessionID string         `json:"sessionId"`
			Prompt    []ContentBlock `json:"prompt"`
		}
		if err := json.Unmarshal(params, &p); err != nil && !isEmptyParams(params) {
			return nil, invalidParams("session/prompt params: %v", err)
		}
		text, err := admitContentBlocks(p.Prompt)
		if err != nil {
			return nil, err
		}
		return s.prompt(p.SessionID, text)

	case "session/cancel":
		// Some clients send it as a request despite the notification spec;
		// answer void best-effort like upstream cancel().
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &p); err == nil && p.SessionID != "" {
			s.cancel(p.SessionID)
		}
		return nil, nil

	default:
		return nil, newACPRequestError(errMethodNotFound, "unknown ACP method: %s", method)
	}
}

// initialize answers the handshake with the exact upstream field set:
// protocolVersion, agentInfo{name,title?,version}, agentCapabilities,
// authMethods. No invented fields. Image input is not implemented yet, so
// promptCapabilities.image advertises false truthfully (upstream
// supportsAcpImagePrompts negative default).
func (s *Server) initialize() (map[string]any, error) {
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"agentInfo": map[string]any{
			"name":    AgentName,
			"title":   AgentTitle,
			"version": AgentVersion,
		},
		"agentCapabilities": map[string]any{
			"loadSession": false,
			"promptCapabilities": map[string]any{
				"audio":           false,
				"embeddedContext": false,
				"image":           false,
			},
		},
		"authMethods": []any{},
	}, nil
}

// newSession validates parameters exactly like upstream validateSessionParams:
// relative cwd and unsupported feature families are explicitly rejected with
// invalidParams instead of silently accepted, then boots one live actor.
// A client-passed mcpServers payload（生态收编）is transparently imported via
// the standard {名称: {定义}} config shapes and mounted into the session's tool
// registry; any import/mount failure rejects the session fail-closed.
func (s *Server) newSession(cwd string, additionalDirs []string, mcpServers json.RawMessage) (map[string]any, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, internalError("the ACP bridge has been disposed")
	}
	if !filepath.IsAbs(cwd) {
		return nil, invalidParams("cwd must be an absolute path: %s", cwd)
	}
	if len(additionalDirs) > 0 {
		return nil, invalidParams("additionalDirectories is not supported")
	}
	inlineSups, err := s.mountInlineMCPServers(mcpServers)
	if err != nil {
		return nil, err
	}

	sessionID := newSessionID()
	header := session.SessionHeader{
		ID:        sessionID,
		CreatedAt: time.Now().UnixMilli(),
		Cwd:       cwd,
		Origin:    "acp",
	}
	ag := agent.NewAgent(header, nil, nil, nil, s.tools, s.llmAdapter,
		"You are DeepSeekHarnessX (DSHX) ACP automation assistant.", "deepseek-chat")
	ag.AttachPluginRuntime(s.plugins)
	// Permission-gated tools wake the ACP permission waterfall; the wire shape
	// is decided inside askPermission (server request -> client response).
	ag.RequestUser = func(reason string, _ []string) (tools.ApprovalDecision, error) {
		return s.askPermission(sessionID, reason)
	}
	ag.Start()

	sub := ag.Subscribe()
	relayCtx, stopRelay := context.WithCancel(context.Background())
	go s.relayEvents(relayCtx, sessionID, ag, sub)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		stopRelay()
		ag.Unsubscribe(sub)
		ag.Stop()
		s.discardMCPMounts(inlineSups) // 本次挂载随会话失败回滚
		return nil, internalError("connection closed during session/new")
	}
	s.sessions[sessionID] = &SessionRecord{agent: ag, stopRelay: stopRelay}
	s.mu.Unlock()
	return map[string]any{"sessionId": sessionID}, nil
}

// mountInlineMCPServers 翻译并挂载 session/new 的原始 mcpServers 参数
// （生态收编透传）：仅接受 {名称: {定义}} 映射形（Claude/Cursor 等配置形状），
// 经 mcp.ImportConfig 统一翻译后挂入共享工具注册表。空载荷/空映射是零操作。
// 超限（字节/服务器数）、形状或翻译错误、挂载错误一律 fail-closed 回
// invalidParams（沿用 ACP 显式拒绝而非静默吞掉的错误风格）。
func (s *Server) mountInlineMCPServers(raw json.RawMessage) ([]*mcp.Supervisor, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	cfg, err := buildInlineMCPConfig(raw)
	if err != nil {
		return nil, invalidParams("%v", err)
	}
	if len(cfg.Servers) == 0 {
		return nil, nil
	}
	mount := s.mountInlineMCP
	if mount == nil {
		mount = mountInlineMCPDefault
	}
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	sups, err := mount(context.Background(), cfg, s.tools, nil)
	if err != nil {
		return nil, invalidParams("%v", err)
	}
	s.mcpSups = append(s.mcpSups, sups...)
	return sups, nil
}

// discardMCPMounts 把一次失败的 session/new 已挂载的服务器自连接级集合撤下
// 并关闭（回滚未落地的挂载）。
func (s *Server) discardMCPMounts(sups []*mcp.Supervisor) {
	if len(sups) == 0 {
		return
	}
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	for _, sup := range sups {
		for i, cur := range s.mcpSups {
			if cur == sup {
				s.mcpSups = append(s.mcpSups[:i], s.mcpSups[i+1:]...)
				break
			}
		}
		_ = sup.Close()
	}
}

// mountInlineMCPDefault 是内联 mcpServers 的生产挂载实现。
func mountInlineMCPDefault(ctx context.Context, cfg *mcp.FileConfig, reg *tools.ToolRegistry, logger *log.Logger) ([]*mcp.Supervisor, error) {
	return mcp.MountConfig(ctx, cfg, reg, logger)
}

// buildInlineMCPConfig 把 session/new 的原始 mcpServers 参数翻译为 FileConfig
// （载荷字节与服务器数封顶；空映射等价于未传）。
func buildInlineMCPConfig(raw json.RawMessage) (*mcp.FileConfig, error) {
	if len(raw) > maxInlineMCPBytes {
		return nil, fmt.Errorf("mcpServers: 载荷 %d 字节超过 %d 上限", len(raw), maxInlineMCPBytes)
	}
	var dict map[string]json.RawMessage
	if err := json.Unmarshal(raw, &dict); err != nil {
		return nil, fmt.Errorf("mcpServers: 只支持 {名称: {定义}} 映射形（Claude/Cursor 配置形状）: %v", err)
	}
	if len(dict) == 0 {
		return &mcp.FileConfig{}, nil // 空映射：零效果
	}
	wrapped := make([]byte, 0, len(raw)+len(`{"mcpServers":}`))
	wrapped = append(wrapped, `{"mcpServers":`...)
	wrapped = append(wrapped, raw...)
	wrapped = append(wrapped, '}')
	cfg, err := mcp.ImportConfig(bytes.NewReader(wrapped), "mcpServers.json")
	if err != nil {
		return nil, err
	}
	if len(cfg.Servers) > maxInlineMCPServers {
		return nil, fmt.Errorf("mcpServers: 服务器数量 %d 超过 %d 上限", len(cfg.Servers), maxInlineMCPServers)
	}
	return cfg, nil
}

// newSessionID returns a fresh random hex identifier.
func newSessionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("acp-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// admitContentBlocks validates the ACP prompt block array and projects it to
// plain model-facing text (upstream admitAcpPrompt under the text-only
// capability): text blocks concatenate in wire order; resource_link renders a
// bracketed reference; image/audio/resource fail fast; empty prompts are
// rejected.
func admitContentBlocks(blocks []ContentBlock) (string, error) {
	if len(blocks) == 0 {
		return "", invalidParams("prompt must contain at least one content block")
	}
	var sb strings.Builder
	for i, block := range blocks {
		switch block.Type {
		case "text":
			sb.WriteString(block.Text)
		case "resource_link":
			fmt.Fprintf(&sb, "\n[resource_link name=%q uri=%q]\n", block.Name, block.URI)
		case "image":
			return "", invalidParams("block %d: inline image prompts were not advertised by this connection", i)
		case "audio":
			return "", invalidParams("block %d: audio prompt content is not supported", i)
		case "resource":
			return "", invalidParams("block %d: embedded resource prompt content is not supported", i)
		default:
			return "", invalidParams("block %d: unsupported ACP prompt content type %q", i, block.Type)
		}
	}
	if strings.TrimSpace(sb.String()) == "" {
		return "", invalidParams("empty prompt")
	}
	return sb.String(), nil
}

// prompt runs one whole-turn automation round trip. It reserves the single
// in-flight slot synchronously, posts the user message into the live actor
// inbox, then waits for full quiescence — the correlated turn's turn/end plus
// every assistant delivery already written — before resolving {stopReason}.
// There is no early admitted response and no stop notification substitute.
func (s *Server) prompt(sessionID, text string) (map[string]any, error) {
	rec, ok := s.lookupSession(sessionID)
	if !ok {
		return nil, invalidParams("unknown session: %s", sessionID)
	}

	rec.mu.Lock()
	if rec.inflight != nil {
		rec.mu.Unlock()
		return nil, invalidParams("a prompt is already in flight for this session")
	}
	inflight := &promptInflight{
		msgID:   fmt.Sprintf("acp-msg-%d-%s", time.Now().UnixNano(), sessionID),
		settled: make(chan struct{}),
	}
	rec.inflight = inflight
	rec.mu.Unlock()

	rec.agent.PostUserMessage(session.UserMessagePayload{
		ID:      inflight.msgID,
		Role:    "user",
		Content: []session.ContentBlock{{Type: "text", Text: text}},
		Source:  session.MessageSource{Kind: "user"},
	})

	// Whole-turn silence gate: resolved by the relay once this message's turn
	// ends after all of its assistant output has been written.
	<-inflight.settled

	rec.mu.Lock()
	if rec.inflight == inflight {
		rec.inflight = nil
	}
	rec.mu.Unlock()
	return map[string]any{"stopReason": string(inflight.stopReason)}, nil
}

// cancel implements AbortTurn semantics: it aborts only the current turn's
// work and never destroys the actor loop. The addressed session stays alive
// and a follow-up session/prompt on the same sessionId works normally. An
// already-settling or idle session ignores the extra call (upstream tolerates
// cancel for unknown/idle sessions). Cancel races any concurrent natural
// settlement through settle-once; whoever lands first decides the stopReason.
func (s *Server) cancel(sessionID string) {
	rec, ok := s.lookupSession(sessionID)
	if !ok {
		return
	}
	rec.mu.Lock()
	inflight := rec.inflight
	rec.mu.Unlock()
	if inflight != nil {
		inflight.settle(StopCancelled)
	}
	// Abort only the live turn context; the actor keeps serving its inbox.
	rec.agent.AbortTurn()
}

// lookupSession returns the record for an existing session.
func (s *Server) lookupSession(sessionID string) (*SessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.sessions[sessionID]
	return rec, ok
}

// ---------------------------------------------------------------------------
// Ordered progress projection: session/update notifications
// ---------------------------------------------------------------------------

// relayEvents forwards one session's event log onto the ACP wire while the
// connection lives. It owns whole-turn settlement for the in-flight prompt:
// agent_message_chunk / agent_thought_chunk / tool_call frames stream during
// the turn; when the prompted message's turn ends, every assistant delivery
// has already been written (the relay is the single ordered consumer of the
// session log), so the prompt settles immediately with the mapped stopReason.
func (s *Server) relayEvents(ctx context.Context, sessionID string, ag *agent.Agent, sub chan *session.SessionEnvelope) {
	defer ag.Unsubscribe(sub)
	for {
		select {
		case <-ctx.Done():
			return
		case env, open := <-sub:
			if !open {
				return
			}
			switch env.Type {
			case session.EventUserMessage:
				// Bind the in-flight prompt to the harness turn that claimed
				// its exact message (upstream agent/inbox/claimed correlation;
				// message ids are unique across turns and producers).
				var payload session.WireMessage
				if err := json.Unmarshal(env.Data, &payload); err != nil || payload.ID == "" {
					continue
				}
				rec, ok := s.lookupSession(sessionID)
				if !ok {
					continue
				}
				rec.mu.Lock()
				inflight := rec.inflight
				rec.mu.Unlock()
				if inflight != nil && payload.ID == inflight.msgID {
					_ = inflight.bind()
				}

			case session.EventAssistantMessage:
				var payload session.AssistantMessagePayload
				if err := json.Unmarshal(env.Data, &payload); err != nil {
					continue
				}
				// Emit committed assistant output (upstream projection):
				// text becomes agent_message_chunk, reasoning becomes
				// agent_thought_chunk; tool/usage blocks stay off the
				// automation wire.
				for _, block := range payload.Message.Content {
					discriminator := ""
					switch block.Type {
					case "text":
						discriminator = "agent_message_chunk"
					case "reasoning":
						discriminator = "agent_thought_chunk"
					}
					if discriminator == "" || block.Text == "" {
						continue
					}
					s.notifyUpdate(sessionID, map[string]any{
						"sessionUpdate": discriminator,
						"content":       map[string]any{"type": "text", "text": block.Text},
					})
				}

			case session.EventToolCall:
				// tool_call family card while the call runs.
				var payload session.ToolCallPayload
				if err := json.Unmarshal(env.Data, &payload); err != nil || payload.CallID == "" {
					continue
				}
				update := map[string]any{
					"sessionUpdate": "tool_call",
					"toolCallId":    payload.CallID,
					"title":         payload.Name,
					"status":        "pending",
				}
				if payload.View != nil {
					update["kind"] = toolViewKind(payload.View.Kind)
				}
				s.notifyUpdate(sessionID, update)

			case session.EventToolResult:
				// tool_call_update family card with the settled outcome.
				var payload session.ToolResultPayload
				if err := json.Unmarshal(env.Data, &payload); err != nil {
					continue
				}
				callID := ""
				isErr := false
				if len(payload.Message.Content) > 0 && payload.Message.Content[0].Type == "tool-result" {
					callID = payload.Message.Content[0].ToolCallID
					isErr = payload.Message.Content[0].IsError
				}
				if callID == "" {
					continue
				}
				status := "completed"
				if isErr || payload.Error != nil {
					status = "failed"
				}
				update := map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    callID,
					"status":        status,
				}
				if payload.View != nil {
					update["kind"] = toolViewKind(payload.View.Kind)
				}
				s.notifyUpdate(sessionID, update)

			case session.EventTurnEnd:
				var endPayload session.TurnEndPayload
				if err := json.Unmarshal(env.Data, &endPayload); err != nil {
					continue
				}
				s.settleOnTurnEnd(sessionID, endPayload.Reason)
			}
		}
	}
}

// settleOnTurnEnd resolves the in-flight prompt once the turn that consumed
// its message ends. The envelope stream is strictly ordered per session, so a
// bound flag at turn/end proves this was the prompt's own turn (its user/message
// was seen above and no other turn could have started since); turns from other
// producers (schedule dispatch, stale queue) find an unbound or absent inflight
// and never settle someone else's prompt.
func (s *Server) settleOnTurnEnd(sessionID string, reason session.TurnEndReason) {
	rec, ok := s.lookupSession(sessionID)
	if !ok {
		return
	}
	rec.mu.Lock()
	inflight := rec.inflight
	rec.mu.Unlock()
	if inflight == nil || !inflight.isBound() {
		return
	}
	inflight.settle(turnEndToStopReason(reason))
}

// turnEndToStopReason maps a harness turn ending to ACP's terminal reason
// vocabulary, mirroring the upstream codec entry for entry.
func turnEndToStopReason(reason session.TurnEndReason) StopReason {
	switch reason.Kind {
	case "completed":
		return StopEndTurn
	case "max-tokens":
		return StopMaxTokens
	// `cancelled` is reserved for explicit client cancellation
	// (`session/cancel`), settled out of band. A turn aborted by a hook or
	// another owner is ordinary quiescence and reports end_turn.
	case "interrupted":
		return StopCancelled
	case "refusal":
		return StopRefusal
	case "aborted", "blocked", "error":
		return StopEndTurn
	default:
		return StopEndTurn
	}
}

// toolViewKind projects a harness card view kind onto the ACP ToolKind
// vocabulary (read/edit/delete/move/execute/search/think/fetch/other).
func toolViewKind(kind string) string {
	switch kind {
	case "terminal":
		return "execute"
	case "diff":
		return "edit"
	default:
		return "other"
	}
}

// notifyUpdate emits one standard session/update notification whose update
// object is discriminated by the sessionUpdate field (transport-only failure
// containment).
func (s *Server) notifyUpdate(sessionID string, update map[string]any) {
	s.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sessionID,
			"update":    update,
		},
	})
}

// ---------------------------------------------------------------------------
// Server->client permission requests
// ---------------------------------------------------------------------------

// askPermission issues one server->client requestPermission request and waits
// for the client's one-shot decision (upstream conn.requestPermission bridge).
// The options carry allow_once/reject_once kinds; the internal channel wake-up
// is reused plumbing, invisible on the wire. A missing or timed-out answer
// cancels the tool call (same fallback as the gateway approval waterfall).
func (s *Server) askPermission(sessionID, reason string) (tools.ApprovalDecision, error) {
	callID := fmt.Sprintf("call_%d", time.Now().UnixNano())
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return tools.ApprovalCancel, nil
	}
	s.nextPermissionID++
	id := fmt.Sprintf("perm-%d-%s", s.nextPermissionID, callID)
	ch := make(chan tools.ApprovalDecision, 1)
	s.pendingPermissions[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pendingPermissions, id)
		s.mu.Unlock()
	}()

	params := map[string]any{
		"sessionId": sessionID,
		"options": []map[string]any{
			{optionIDField: optAllowOnce, nameField: "Allow once", kindField: kindAllowOnce},
			{optionIDField: optRejectOnce, nameField: "Reject", kindField: kindRejectOnce},
		},
		"toolCall": map[string]any{"toolCallId": callID},
	}
	if reason != "" {
		// Optional human-readable detail alongside the schema shape; clients
		// that ignore unknown members stay conformant.
		params["reason"] = reason
	}
	s.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "requestPermission",
		"params":  params,
	})

	timer := time.NewTimer(permissionTimeout)
	defer timer.Stop()
	select {
	case decision := <-ch:
		return decision, nil
	case <-timer.C:
		return tools.ApprovalCancel, nil
	}
}

// requestPermission params member names (upstream RequestPermissionRequest).
const (
	optionIDField = "optionId"
	nameField     = "name"
	kindField     = "kind"

	kindAllowOnce  = "allow_once"
	kindRejectOnce = "reject_once"
)

// resolvePermission completes one outstanding permission request from the
// client's JSON-RPC response frame (upstream RequestResult outcome shape:
// {outcome:{outcome:"selected",optionId}} or {outcome:{outcome:"cancelled"}}).
func (s *Server) resolvePermission(id string, rawResult json.RawMessage, rpcErr *rpcError) {
	// Delete-first guarantees exactly one resolver ever answers the channel.
	s.mu.Lock()
	ch, ok := s.pendingPermissions[id]
	delete(s.pendingPermissions, id)
	s.mu.Unlock()
	if !ok {
		return
	}
	decision := tools.ApprovalCancel
	if rpcErr == nil {
		var out struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		if err := json.Unmarshal(rawResult, &out); err == nil &&
			out.Outcome.Outcome == outcomeSelected && out.Outcome.OptionID == optAllowOnce {
			decision = tools.ApprovalAllowOnce
		} else if err == nil && out.Outcome.Outcome == outcomeSelected {
			decision = tools.ApprovalDeny
		}
	}
	select {
	case ch <- decision:
	default:
	}
}

// write serializes one frame as a single NDJSON line under the write lock.
func (s *Server) write(frame map[string]any) {
	data, err := json.Marshal(frame)
	if err != nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.writer.Write(append(data, '\n'))
}
