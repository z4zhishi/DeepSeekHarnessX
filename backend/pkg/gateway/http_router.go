package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dsh-go/pkg/agent"
	"dsh-go/pkg/credential"
	"dsh-go/pkg/feedback"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/plugin"
	"dsh-go/pkg/session"
	"dsh-go/pkg/settings"
	"dsh-go/pkg/storage"
	"dsh-go/pkg/subagent"
	"dsh-go/pkg/tools"
	"dsh-go/pkg/workspace"
)

// PluginCtl is the plugin control surface gateway RPCs consume.
// *plugin.Registry satisfies it once ListInfo/InstallFromPath/Uninstall/
// SetEnabled land.
type PluginCtl interface {
	ListInfo() []plugin.PluginInfo
	InstallFromPath(ctx context.Context, src, destDir string) (plugin.PluginInfo, error)
	Uninstall(name string) error
	SetEnabled(name string, enabled bool) error
}

// SessionStore is the storage surface the gateway consumes. Both the
// bbolt-backed and the schema-17 SQLite stores satisfy it, so the gateway
// stays backend-agnostic.
type SessionStore interface {
	ListSessions() ([]session.SessionHeader, error)
	PutSession(header *session.SessionHeader) error
	GetEvents(sessionID string, fromSeq int) ([]session.SessionEnvelope, error)
	AppendEvents(meta *session.SessionHeader, events []*session.SessionEnvelope) error
}

// Server represents the integrated HTTP and WebSocket gateway.
type Server struct {
	Store      SessionStore
	Hub        *DownlinkHub
	Tools      *tools.ToolRegistry
	LlmAdapter llm.LlmAdapter
	// Version is reported by host.describe; injected by the host from the
	// build-time main.version so the Godot header/jobs panels show the real
	// build instead of a hardcoded constant.
	Version string
	// PCKHash is the SHA-256 of the frontend PCK embedded in the running host.
	// Empty means the host could not expose an embedded GUI payload.
	PCKHash string
	// Workspaces is the workspace root(s) served by workspace.list. nil falls
	// back to the process working directory.
	Workspaces []string
	// WorkspaceMgr drives the real workspace backend (workspace.list /
	// workspace.create). When nil the server falls back to the legacy
	// Workspaces-roots behavior for backward compatibility.
	WorkspaceMgr *workspace.Manager
	// Feedback is the per-message rating+note sidecar store backing the
	// feedback.list / feedback.put / feedback.delete RPCs. nil reports an
	// empty feedback surface.
	Feedback *feedback.Store
	// HookBus is the shared plugin event bus the agent loop dispatches CC
	// hooks through. nil disables the hooks runtime for created sessions.
	HookBus *plugin.EventBus
	// Hooks is the parsed hooks.json configuration (shared with every created
	// session). nil leaves the runtime inert even when HookBus is set.
	Hooks *plugin.Hooks
	// Settings is the settings backend driving settings.describe / settings.mutate.
	// nil reports an empty, read-only settings surface.
	Settings *settings.Manager
	// Credentials is the credential-reference backend (settings.credentials).
	// nil reports every reference unconfigured/read-only.
	Credentials *credential.Manager
	// Model is the configured default model id (e.g. "deepseek-v4-flash").
	// It is reported by llm.models and seeds the token meter's context window
	// for session.context. Empty falls back to the active provider profile, then
	// the catalog's first model. Unknown ids are kept (not clamped to catalog).
	Model string
	// PluginDir is the on-disk plugin install root passed to InstallFromPath.
	PluginDir string
	// Plugins is the plugin control surface (typically *plugin.Registry).
	Plugins PluginCtl
	agents  map[string]*agent.Agent
	// subagents is the process-level subagent manager whose lifecycle events
	// are relayed to the host downlink (Godot subagent tree).
	subagents *subagent.Manager
	// pendingApprovals tracks in-flight one-shot permission requests keyed by callId;
	// the Godot GUI answers via the approval.respond RPC (mirrors ACP bridge).
	pendingApprovals map[string]*pendingApproval
	mu               sync.RWMutex

	// inboundEnsureSessionFn and inboundTurnFn are deterministic seams for the
	// HTTP protocol tests. Production leaves them nil and uses the live actor
	// implementation; tests can drive terminal turn outcomes without waiting on
	// an actor or a real provider timeout.
	inboundEnsureSessionFn func(string) (*agent.Agent, string, bool, error)
	inboundTurnFn          func(context.Context, *agent.Agent, string, func(*session.SessionEnvelope)) error
}

// pendingApproval is one in-flight host/permission-request. sessionID lets
// session.abort/stop cancel every prompt belonging to that session so a GUI
// Stop cannot leave the actor parked inside askApproval until the 60s timeout.
type pendingApproval struct {
	sessionID string
	ch        chan tools.ApprovalDecision
}

// NewServer creates a new API server.
func NewServer(store SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter) *Server {
	return NewServerWithVersion(store, toolReg, adapter, "dev")
}

// NewServerWithVersion creates a new API server with an explicit host version.
func NewServerWithVersion(store SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, version string) *Server {
	s := &Server{
		Store:            store,
		Hub:              NewDownlinkHub(),
		Tools:            toolReg,
		LlmAdapter:       adapter,
		Version:          version,
		agents:           make(map[string]*agent.Agent),
		pendingApprovals: make(map[string]*pendingApproval),
	}
	s.Hub.Replay = func(sessionID string, fromSeq int) []session.SessionEnvelope {
		if s.Store == nil || sessionID == "" {
			return nil
		}
		events, err := s.Store.GetEvents(sessionID, fromSeq)
		if err != nil {
			return nil
		}
		return events
	}
	// Wire the small-model review seam for the "auto" (review) approval policy.
	// The gateway's LlmAdapter answers a one-shot safety judgment; when the
	// adapter is nil or the call fails, the pipeline escalates to the user.
	if toolReg != nil {
		toolReg.ReviewTool = s.reviewToolCall
	}
	return s
}

// reviewToolCall asks the configured small review model whether one tool call
// is safe to auto-run. It returns allow / deny / uncertain (escalate) based on
// a single short completion; any error resolves to uncertain so the user is
// always asked rather than silently auto-allowed.
func (s *Server) reviewToolCall(sessionID, reviewModel, toolName, argsJSON string, timeout time.Duration) tools.ReviewVerdict {
	if s.LlmAdapter == nil || reviewModel == "" {
		return tools.ReviewUncertain
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req := llm.ModelRequest{
		Model:           reviewModel,
		SessionID:       sessionID,
		Purpose:         "approval-review",
		ReasoningEffort: "off",
		System:          "You judge whether one tool call is safe to auto-run for the local user. Answer with exactly one word: allow, deny, or uncertain. allow = read-only or contained in the workspace; deny = destructive or escapes the workspace; uncertain = needs the user.",
		Messages: []session.ModelMessage{
			{Role: "user", Content: []session.ContentBlock{{
				Type: "text",
				Text: fmt.Sprintf("Tool: %s\nArguments: %s\nVerdict (allow/deny/uncertain):", toolName, argsJSON),
			}}},
		},
	}
	chunks, errs := s.LlmAdapter.Stream(ctx, req)
	var out strings.Builder
	done := false
	for !done {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				// Drain any error (usually nil on clean finish).
				select {
				case <-errs:
				default:
				}
				done = true
				continue
			}
			if c.Text != "" {
				out.WriteString(c.Text)
			}
		case <-ctx.Done():
			return tools.ReviewUncertain
		}
	}
	return parseReviewVerdict(out.String())
}

func parseReviewVerdict(raw string) tools.ReviewVerdict {
	verdict := strings.ToLower(strings.TrimSpace(raw))
	// Accept only a standalone verdict token. Substrings such as "disallow"
	// and phrases with contradictory/negative language must never auto-allow.
	fields := strings.FieldsFunc(verdict, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '.' || r == ',' || r == ':' || r == ';' || r == '!' || r == '?' || r == '`' || r == '"' || r == '\''
	})
	if len(fields) != 1 {
		return tools.ReviewUncertain
	}
	switch fields[0] {
	case "allow":
		return tools.ReviewAllow
	case "deny":
		return tools.ReviewDeny
	case "uncertain":
		return tools.ReviewUncertain
	default:
		return tools.ReviewUncertain
	}
}

// Routes sets up all HTTP routes matching DSH gateway specs.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// WebSocket downlinks
	mux.HandleFunc("/api/events/mux", s.Hub.HandleMux)
	mux.HandleFunc("/api/events/host", s.Hub.HandleHost)

	// Unary RPC dispatcher（在 Host/Origin 信任栅栏之后）
	mux.HandleFunc("/api/", s.handleRPC)

	// Inbound native protocols (DSHX as OpenAI/Anthropic-compatible server).
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/v1/messages", s.handleAnthropicMessages)

	// CSRF/Host 信任栅栏：网关只服务 loopback 客户端；全路由统一请求体限幅
	// （/api/* RPC 与 /v1/* 入站共用上限；WS upgrade 空体不受影响）。
	return s.trustGuard(limitRequestBody(mux))
}

// maxRequestBodyBytes caps every request body (upstream parity: 300 MiB) so a
// runaway client cannot exhaust memory on decode. Package-level var only so
// tests can exercise the limit without allocating 300 MiB.
var maxRequestBodyBytes int64 = 300 << 20

// limitRequestBody wraps every route with http.MaxBytesReader. Overshoot
// surfaces as *http.MaxBytesError at read time: handleRPC answers 413, the
// /v1/* handlers answer their existing 400-on-read-error path.
func limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	// CORS 动态回显（与信任栅栏联动）：能携带 Origin 走到这里说明已通过
	// ④ 的精确一致栅栏，安全回显；无 Origin 的本地客户端无需该头。
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	}

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	method := strings.TrimPrefix(r.URL.Path, "/api/")

	writeRPCError := func(status int, format string, args ...any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "server-response",
			"rpcId": fmt.Sprintf("rpc-%d", time.Now().UnixNano()),
			"result": map[string]any{
				"ok":    false,
				"error": map[string]any{"message": fmt.Sprintf(format, args...)},
			},
		})
	}

	// POST RPC 校验 Content-Type（带参数如 ;charset=utf-8 亦可）；缺失时容忍
	// Godot/curl 等最小客户端——跨站滥用由信任栅栏拦截，非本校验职责。
	if r.Method == http.MethodPost {
		if ct := r.Header.Get("Content-Type"); ct != "" {
			mt, _, err := mime.ParseMediaType(ct)
			if err != nil || mt != "application/json" {
				writeRPCError(http.StatusUnsupportedMediaType, "Content-Type must be application/json, got %q", ct)
				return
			}
		}
	}

	var body map[string]any
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			// Malformed payloads must not silently dispatch as empty requests.
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeRPCError(http.StatusRequestEntityTooLarge, "request body exceeds %d bytes", mbe.Limit)
				return
			}
			writeRPCError(http.StatusBadRequest, "invalid JSON body: %v", err)
			return
		}
	}

	rpcID, _ := body["rpcId"].(string)
	if rpcID == "" {
		rpcID = fmt.Sprintf("rpc-%d", time.Now().UnixNano())
	}

	payload, _ := body["payload"].(map[string]any)
	if payload == nil {
		payload = map[string]any{}
	}

	res, err := s.dispatch(r.Context(), method, payload)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "server-response",
			"rpcId": rpcID,
			"result": map[string]any{
				"ok": false,
				"error": map[string]any{
					"message": err.Error(),
				},
			},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "server-response",
		"rpcId": rpcID,
		"result": map[string]any{
			"ok":    true,
			"value": res,
		},
	})
}

func (s *Server) dispatch(ctx context.Context, method string, payload map[string]any) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	switch method {
	case "host.describe":
		return map[string]any{
			"ready":       true,
			"engine":      "dsh-go-godot",
			"name":        "DSHX",
			"backend":     "dsh-go-godot",
			"protocol":    "http+ws",
			"version":     s.hostVersion(),
			"runtime":     "go1.25",
			"activeHub":   true,
			"environment": "desktop",
		}, nil

	case "session.list":
		if s.Store != nil {
			return s.Store.ListSessions()
		}
		return []session.SessionHeader{}, nil

	case "session.create":
		id, _ := payload["id"].(string)
		if id == "" {
			id = fmt.Sprintf("session-%d", time.Now().UnixNano())
		}
		cwd, _ := payload["cwd"].(string)
		preset, _ := payload["preset"].(string)

		header := session.SessionHeader{
			Version:     0,
			ID:          id,
			CreatedAt:   time.Now().UnixMilli(),
			Cwd:         cwd,
			AgentPreset: preset,
		}

		if s.Store != nil {
			_ = s.Store.PutSession(&header)
		}

		s.spawnAgent(header)
		s.Hub.BroadcastHostEvent("host/session-added", header)
		return header, nil

	case "session.resume":
		// Re-attach a stored session's actor. A live actor is reused; after
		// session.stop the dead map entry is replaced (NewAgent seeds seq).
		sessionID, _ := payload["sessionId"].(string)
		if sessionID == "" {
			return nil, fmt.Errorf("session.resume requires a sessionId")
		}
		header, ok := s.lookupHeader(sessionID)
		if !ok {
			return nil, fmt.Errorf("session not found: %s", sessionID)
		}
		s.mu.RLock()
		ag, exists := s.agents[sessionID]
		s.mu.RUnlock()
		if exists && ag.Alive() {
			return header, nil
		}
		s.spawnAgent(header)
		return header, nil

	case "session.history":
		sessionID, _ := payload["sessionId"].(string)
		fromSeq := 0
		if f, ok := payload["fromSeq"].(float64); ok {
			fromSeq = int(f)
		}

		if s.Store != nil {
			events, err := s.Store.GetEvents(sessionID, fromSeq)
			if err != nil {
				return nil, err
			}
			if events == nil {
				events = []session.SessionEnvelope{}
			}
			return events, nil
		}
		return []session.SessionEnvelope{}, nil

	case "context.limit.get":
		limit, source := s.resolvedContextLimit()
		return map[string]any{"limitTokens": limit, "source": source}, nil

	case "context.limit.set":
		reset, _ := payload["reset"].(bool)
		if reset {
			if s.Settings == nil {
				return nil, fmt.Errorf("settings service unavailable")
			}
			if _, err := s.Settings.Mutate("general", []settings.Op{{Op: "unset", Path: []string{"contextLimitTokens"}}}); err != nil {
				return nil, err
			}
			limit, source := s.resolvedContextLimit()
			return map[string]any{"limitTokens": limit, "source": source}, nil
		}
		limitK, ok := numberValue(payload["limitK"])
		if !ok {
			return nil, fmt.Errorf("context.limit.set requires a numeric limitK")
		}
		tokens := limitK * 1000
		if tokens < 1000 || float64(int(tokens)) != tokens {
			return nil, fmt.Errorf("context.limit.set limitK must convert to an integer token limit >= 1000")
		}
		if s.Settings == nil {
			return nil, fmt.Errorf("settings service unavailable")
		}
		if _, err := s.Settings.Mutate("general", []settings.Op{{Op: "set", Path: []string{"contextLimitTokens"}, Value: int(tokens)}}); err != nil {
			return nil, err
		}
		limit, source := s.resolvedContextLimit()
		return map[string]any{"limitTokens": limit, "source": source}, nil

	case "session.context":
		// {sessionId} -> {tokenUsage, contextPressure, projectedTokens,
		// messageCount, breakdown, contextLimit}
		// Prices the session transcript with the shared llm.Meter heuristic so
		// the frontend can render context occupancy. The meter's ContextLimit is
		// the selected model's context window (falling back to the default model).
		sessionID, _ := payload["sessionId"].(string)
		if sessionID == "" {
			return nil, fmt.Errorf("session.context requires a sessionId")
		}
		if s.Store == nil {
			return nil, fmt.Errorf("context service unavailable")
		}
		events, err := s.Store.GetEvents(sessionID, 0)
		if err != nil {
			return nil, err
		}
		if events == nil {
			events = []session.SessionEnvelope{}
		}
		metrics, err := (llm.Meter{ContextLimit: s.resolvedContextLimitTokens()}).Measure(events)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"tokenUsage":       metrics.TokenUsage,
			"contextPressure":  metrics.ContextPressure,
			"contextLimit":     metrics.ContextLimit,
			"projectedTokens":  metrics.ProjectedTokens,
			"messageCount":     metrics.MessageCount,
			"breakdown":        metrics.Breakdown,
			"tokenUsageInput":  metrics.TokenUsage.InputTokens,
			"tokenUsageOutput": metrics.TokenUsage.OutputTokens,
		}, nil

	case "session.policy":
		// {sessionId} -> {mode, sandbox, approval, reviewModel, preset}
		// Echoes the session's resolved permission policy so the GUI's access
		// dropdown always reflects the real backend state (not a stale default).
		sessionID, _ := payload["sessionId"].(string)
		if sessionID == "" {
			return nil, fmt.Errorf("session.policy requires a sessionId")
		}
		if s.Tools == nil || s.Tools.Policy == nil {
			return nil, fmt.Errorf("policy service unavailable")
		}
		pol := s.Tools.Policy.Get(sessionID)
		return map[string]any{
			"mode":        string(pol.Mode()),
			"sandbox":     string(pol.Sandbox),
			"approval":    string(pol.Approval),
			"reviewModel": pol.ReviewModel,
			"preset":      pol.Preset,
		}, nil

	case "session.effort":
		// {sessionId, effort} -> {effort}
		// Sets the per-session reasoning effort ("off"|"low"|"high"|"max")
		// that the agent applies to the next model request. Mirrors the TUI's
		// tunedAdapter override so the Godot header ⚙ popup and /thinking stay
		// consistent. get leaves it unchanged (empty effort => read back).
		sessionID, _ := payload["sessionId"].(string)
		effort, _ := payload["effort"].(string)
		if sessionID == "" {
			return nil, fmt.Errorf("session.effort requires a sessionId")
		}
		ag, err := s.ensureLiveAgent(sessionID)
		if err != nil {
			return nil, err
		}
		if effort != "" {
			if eff, ok := normalizeEffort(effort); ok {
				ag.Effort = eff
			}
		}
		return map[string]any{"effort": ag.Effort}, nil

	case "session.prompt":
		sessionID, _ := payload["sessionId"].(string)
		text, _ := payload["text"].(string)
		ag, err := s.ensureLiveAgent(sessionID)
		if err != nil {
			return nil, err
		}
		ag.PostUserMessage(session.UserMessagePayload{
			ID:   fmt.Sprintf("msg-%d", time.Now().UnixNano()),
			Role: "user",
			Content: []session.ContentBlock{
				{Type: "text", Text: text},
			},
			Source: session.MessageSource{Kind: "user"},
		})
		return map[string]any{"admitted": true}, nil

	case "session.steer":
		// Mid-turn next-step interrupt. A dead actor has no turn to interrupt,
		// so we respawn and treat the text as a fresh user message.
		sessionID, _ := payload["sessionId"].(string)
		text, _ := payload["text"].(string)
		s.mu.RLock()
		ag, exists := s.agents[sessionID]
		s.mu.RUnlock()
		if !exists || !ag.Alive() {
			ag, err := s.ensureLiveAgent(sessionID)
			if err != nil {
				return nil, err
			}
			ag.PostUserMessage(session.UserMessagePayload{
				ID:   fmt.Sprintf("msg-%d", time.Now().UnixNano()),
				Role: "user",
				Content: []session.ContentBlock{
					{Type: "text", Text: text},
				},
				Source: session.MessageSource{Kind: "user"},
			})
			return map[string]any{"steered": true, "respawned": true}, nil
		}
		ag.PostNextStep(session.ContentBlock{Type: "text", Text: text})
		return map[string]any{"steered": true}, nil

	case "session.abort":
		// Soft abort: cancel the in-flight turn without destroying the actor.
		// GUI Stop should use this; session.stop remains a hard teardown.
		sessionID, _ := payload["sessionId"].(string)
		if sessionID == "" {
			return nil, fmt.Errorf("session.abort requires a sessionId")
		}
		s.mu.RLock()
		ag, ok := s.agents[sessionID]
		s.mu.RUnlock()
		if !ok {
			if _, found := s.lookupHeader(sessionID); !found {
				return nil, fmt.Errorf("session not found: %s", sessionID)
			}
			return map[string]any{"aborted": true}, nil
		}
		ag.AbortTurn()
		s.cancelSessionApprovals(sessionID)
		return map[string]any{"aborted": true}, nil

	case "session.stop":
		// Hard Stop: cancel the actor context and destroy the live loop.
		// Teardown/shutdown only. GUI conversation Stop must use session.abort
		// so the next prompt can reuse the same actor.
		sessionID, _ := payload["sessionId"].(string)
		s.mu.RLock()
		ag, ok := s.agents[sessionID]
		s.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("session not found or active: %s", sessionID)
		}
		ag.Stop()
		s.cancelSessionApprovals(sessionID)
		return map[string]any{"stopped": true}, nil

	case "command.list":
		cmds := []map[string]any{}
		if s.Tools != nil && s.Tools.Commands != nil {
			for _, def := range s.Tools.Commands.List() {
				cmds = append(cmds, map[string]any{
					"name":        def.Name,
					"description": def.Description,
				})
			}
		}
		return map[string]any{"commands": cmds}, nil

	case "session.command":
		// Slash-command execution through the shared registry: the lifecycle
		// pair command/run -> command/done is appended to the session log via
		// the owning agent (upstream dsh-commands execute()).
		sessionID, _ := payload["sessionId"].(string)
		line, _ := payload["line"].(string)
		if sessionID == "" || line == "" {
			return nil, fmt.Errorf("sessionId and line are required")
		}
		s.mu.RLock()
		ag, ok := s.agents[sessionID]
		s.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("session not found or active: %s", sessionID)
		}
		if s.Tools == nil || s.Tools.Commands == nil {
			return nil, fmt.Errorf("command registry unavailable")
		}
		res := s.Tools.Commands.Execute(tools.CommandInvocation{
			SessionID: sessionID,
			Cwd:       ag.Header.Cwd,
			Emit: func(eventType string, payload any) {
				_, _ = ag.EmitEvent(eventType, payload)
			},
			EmitSeq: func(eventType string, payload any) (int, error) {
				env, err := ag.EmitEvent(eventType, payload)
				if err != nil {
					return 0, err
				}
				return env.Seq, nil
			},
			Policy: s.Tools.Policy,
		}, line)
		if res == nil {
			return nil, fmt.Errorf("unknown command or malformed line: %s", line)
		}
		return map[string]any{"kind": res.Kind, "text": res.Text, "sourceEventSeq": res.SourceSeq}, nil

	case "workspace.list":
		return s.workspaceList(), nil

	case "workspace.create":
		// Create a new workspace at an explicit path (backend-driven mkdir when
		// absent). Requires the manager; a nil manager reports an unavailable
		// service instead of silently succeeding.
		if s.WorkspaceMgr == nil {
			return nil, fmt.Errorf("workspace service unavailable")
		}
		path, _ := payload["path"].(string)
		if path == "" {
			return nil, fmt.Errorf("workspace.create requires a path")
		}
		ws, err := s.WorkspaceMgr.Create(path)
		if err != nil {
			return nil, err
		}
		return ws, nil

	case "feedback.list":
		// {sessionId} -> {items:[{messageId, rating, note?, version, createdAt, updatedAt}]}
		if s.Feedback == nil {
			return map[string]any{"items": []any{}}, nil
		}
		sessionID, _ := payload["sessionId"].(string)
		return map[string]any{"items": s.Feedback.List(sessionID)}, nil

	case "feedback.put":
		// {sessionId, messageId, rating, note?, version?} -> {item}
		// On a version conflict the authoritative current item is returned in
		// the error path; callers diff and retry with the fresh token.
		if s.Feedback == nil {
			return nil, fmt.Errorf("feedback service unavailable")
		}
		sessionID, _ := payload["sessionId"].(string)
		messageID, _ := payload["messageId"].(string)
		rating, _ := payload["rating"].(string)
		note, hasNote := payload["note"]
		ifVersion, _ := payload["version"].(string)
		if sessionID == "" || messageID == "" {
			return nil, fmt.Errorf("feedback.put requires sessionId and messageId")
		}
		req := feedback.PutRequest{
			SessionID: sessionID,
			MessageID: messageID,
			Rating:    feedback.Rating(rating),
			HasNote:   hasNote,
			IfVersion: ifVersion,
		}
		if hasNote {
			req.Note, _ = note.(string)
		}
		item, err := s.Feedback.Put(req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"item": item}, nil

	case "feedback.delete":
		// {sessionId, messageId, version?} -> {ok}
		if s.Feedback == nil {
			return nil, fmt.Errorf("feedback service unavailable")
		}
		sessionID, _ := payload["sessionId"].(string)
		messageID, _ := payload["messageId"].(string)
		ifVersion, _ := payload["version"].(string)
		if err := s.Feedback.Delete(feedback.DeleteRequest{
			SessionID: sessionID,
			MessageID: messageID,
			IfVersion: ifVersion,
		}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil

	case "settings.describe":
		// {namespaces:[{ns, base, user, revision, schema, writable}], writable, hasDocument}
		writable := s.Settings != nil
		namespaces := []settings.Descriptor{}
		hasDocument := false
		if s.Settings != nil {
			namespaces = s.Settings.Describe()
			hasDocument = settingsHasDocument(s.Settings)
		}
		return map[string]any{
			"namespaces":  namespaces,
			"writable":    writable,
			"hasDocument": hasDocument,
		}, nil

	case "settings.mutate":
		// {ns, ops:[{op:"set"|"unset", path, value?}], expectedRevision?} -> {revision}
		if s.Settings == nil {
			return nil, fmt.Errorf("settings service unavailable")
		}
		ns, _ := payload["ns"].(string)
		var ops []settings.Op
		rawOps, ok := payload["ops"].([]any)
		if !ok {
			return nil, fmt.Errorf("settings.mutate requires an ops array")
		}
		for _, r := range rawOps {
			m, ok := r.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("settings.mutate ops entries must be objects")
			}
			op, _ := m["op"].(string)
			path := toStringSlice(m["path"])
			value, hasVal := m["value"]
			if op == "set" && !hasVal {
				return nil, fmt.Errorf("settings.mutate set op requires a value")
			}
			ops = append(ops, settings.Op{Op: op, Path: path, Value: value})
		}
		// Optional optimistic-concurrency token: the revision the caller last
		// saw via settings.describe. Forwarded to the Manager; on mismatch the
		// write is rejected with a conflict error whose message carries both
		// the expected and the actual revision (upstream SETTINGS_CONFLICT).
		var expectRevision []int
		if raw, present := payload["expectedRevision"]; present && raw != nil {
			num, ok := raw.(float64) // encoding/json decodes JSON numbers as float64
			if !ok {
				return nil, fmt.Errorf("settings.mutate expectedRevision must be a number")
			}
			expectRevision = append(expectRevision, int(num))
		}
		rev, err := s.Settings.Mutate(ns, ops, expectRevision...)
		if err != nil {
			return nil, err
		}
		return map[string]any{"revision": rev}, nil

	case "settings.credentials.describe":
		// {configured, source, writable} for one reference.
		if s.Credentials == nil {
			return map[string]any{"configured": false, "writable": false}, nil
		}
		ref, _ := payload["ref"].(string)
		if !credential.IsRefName(ref) {
			return nil, fmt.Errorf("invalid credential reference %q", ref)
		}
		info, err := s.Credentials.Describe(ref)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"configured": info.Configured,
			"source":     info.Source,
			"writable":   info.Writable,
		}, nil

	case "settings.credentials.set":
		if s.Credentials == nil {
			return nil, fmt.Errorf("credentials service unavailable")
		}
		ref, _ := payload["ref"].(string)
		value, _ := payload["value"].(string)
		if err := s.Credentials.Set(ref, value); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil

	case "settings.credentials.unset":
		if s.Credentials == nil {
			return nil, fmt.Errorf("credentials service unavailable")
		}
		ref, _ := payload["ref"].(string)
		if err := s.Credentials.Unset(ref); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil

	case "settings.credentials.list":
		if s.Credentials == nil {
			return map[string]any{"refs": []any{}}, nil
		}
		// Enumerate every reference the writable store knows; values excluded.
		refs := s.Credentials.ListRefs()
		return map[string]any{"refs": refs}, nil

	case "llm.models":
		// { } -> {models:[{id, name, contextWindow, modalities}], selected, fetchError?}
		// Live provider listing first, then any DefaultModels id not already present.
		models := llm.DefaultModels
		var fetchError string
		active, profiles := s.providerState()
		if p, ok := profiles[active]; ok && active != "" {
			protocol := p.Protocol
			if protocol == "" || protocol == "deepseek" {
				protocol = llm.ProtocolOpenAICompletions
			}
			key := ""
			if s.Credentials != nil && p.APIKeyRef != "" {
				if k, kerr := s.Credentials.ResolveValue(p.APIKeyRef); kerr == nil {
					key = k
				}
			}
			fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			live, err := llm.FetchModelsFor(fctx, protocol, p.BaseURL, key, nil)
			cancel()
			if err != nil {
				fetchError = err.Error()
			} else {
				models = mergeModelCatalog(live, llm.DefaultModels)
			}
		}
		out := map[string]any{"models": modelInfoMaps(models), "selected": s.configuredModel()}
		if fetchError != "" {
			out["fetchError"] = fetchError
		}
		return out, nil

	case "model.set":
		// {model} -> {selected}：持久化默认模型并重配 adapter，使选择立即生效。
		model, _ := payload["model"].(string)
		if model == "" {
			return nil, fmt.Errorf("model.set requires a model id")
		}
		if s.Settings != nil {
			if _, err := s.Settings.Mutate("general", []settings.Op{
				{Op: "set", Path: []string{"model"}, Value: model},
			}); err != nil {
				return nil, err
			}
		}
		s.Model = model
		switch a := s.LlmAdapter.(type) {
		case *llm.Router:
			a.SetModel(model)
		case *llm.DeepSeekAdapter:
			a.SetModel(model)
		}
		s.mu.Lock()
		for _, ag := range s.agents {
			if ag != nil {
				ag.ModelName = model
			}
		}
		s.mu.Unlock()
		return map[string]any{"selected": s.configuredModel()}, nil

	case "provider.describe":
		// { } -> {active, profiles:[...], usable}
		s.SeedDefaultProvider()
		return s.providerDescribeResp(), nil

	case "provider.set":
		// {id, name?, protocol?, baseUrl?, model?, apiKeyRef?, apiKey?, setActive?}
		id, _ := payload["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("provider.set requires an id")
		}
		// Read existing profile (or create a default-shaped one).
		_, profiles := s.providerState()
		cur := map[string]any{"id": id, "name": id, "protocol": llm.ProtocolOpenAICompletions, "baseUrl": llm.DefaultDeepSeekBaseURL, "model": llm.DefaultDeepSeekModel, "apiKeyRef": "DEEPSEEK_API_KEY"}
		if ex, ok := profiles[id]; ok {
			cur = ex.Raw
		}
		// Apply overrides from the payload.
		if v, ok := payload["name"].(string); ok && v != "" {
			cur["name"] = v
		}
		if v, ok := payload["protocol"].(string); ok && v != "" {
			cur["protocol"] = v
		}
		if v, ok := payload["baseUrl"].(string); ok && v != "" {
			cur["baseUrl"] = v
		}
		if v, ok := payload["model"].(string); ok && v != "" {
			cur["model"] = v
		}
		keyRef, _ := cur["apiKeyRef"].(string)
		if keyRef == "" {
			keyRef = "DEEPSEEK_API_KEY"
			cur["apiKeyRef"] = keyRef
		}
		// Persist the profile under provider.profiles.<id>.
		if s.Settings != nil {
			if _, err := s.Settings.Mutate("provider", []settings.Op{
				{Op: "set", Path: []string{"profiles", id}, Value: cur},
			}); err != nil {
				return nil, err
			}
		}
		// Persist the API key into the credential store if provided.
		if v, ok := payload["apiKey"].(string); ok && v != "" {
			if s.Credentials != nil {
				if err := s.Credentials.Set(keyRef, v); err != nil {
					return nil, err
				}
			}
		}
		setActive, _ := payload["setActive"].(bool)
		if setActive {
			if _, err := s.setActiveProvider(id); err != nil {
				return nil, err
			}
		}
		return s.providerDescribeResp(), nil

	case "provider.apply":
		// Reconfigure the adapter from the active profile (fast switch).
		active, _ := s.providerState()
		if active == "" {
			return nil, fmt.Errorf("no active provider configured")
		}
		if _, err := s.applyProviderConfig(active); err != nil {
			return nil, err
		}
		return s.providerDescribeResp(), nil

	case "provider.delete":
		// {id} -> provider.describe：从 provider 命名空间移除一个 profile。
		id, _ := payload["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("provider.delete requires an id")
		}
		if s.Settings != nil {
			if _, err := s.Settings.Mutate("provider", []settings.Op{
				{Op: "unset", Path: []string{"profiles", id}},
			}); err != nil {
				return nil, err
			}
		}
		active, _ := s.providerState()
		if active == id {
			// 删除了当前 active：清空 active。
			if s.Settings != nil {
				if _, err := s.Settings.Mutate("provider", []settings.Op{
					{Op: "unset", Path: []string{"active"}},
				}); err != nil {
					return nil, err
				}
			}
		}
		return s.providerDescribeResp(), nil

	case "provider.models":
		// {profileId?} -> {models, selected}：从目标 profile 的 protocol/baseUrl 拉取模型。
		profileID, _ := payload["profileId"].(string)
		active, profiles := s.providerState()
		if profileID == "" {
			profileID = active
		}
		if profileID == "" {
			return map[string]any{"models": llm.DefaultModels, "selected": s.configuredModel()}, nil
		}
		p, ok := profiles[profileID]
		if !ok {
			return map[string]any{"models": llm.DefaultModels, "selected": s.configuredModel()}, nil
		}
		protocol := p.Protocol
		if protocol == "" || protocol == "deepseek" {
			protocol = llm.ProtocolOpenAICompletions
		}
		key := ""
		if s.Credentials != nil && p.APIKeyRef != "" {
			if k, kerr := s.Credentials.ResolveValue(p.APIKeyRef); kerr == nil {
				key = k
			}
		}
		fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		models, err := llm.FetchModelsFor(fctx, protocol, p.BaseURL, key, nil)
		if err != nil {
			return map[string]any{"models": llm.DefaultModels, "selected": s.configuredModel(), "fetchError": err.Error()}, nil
		}
		if len(models) == 0 {
			models = llm.DefaultModels
		}
		return map[string]any{"models": models, "selected": s.configuredModel()}, nil

	case "jobs.list":
		sessionID, _ := payload["sessionId"].(string)
		if sessionID == "" {
			return nil, fmt.Errorf("sessionId is required")
		}
		return map[string]any{
			"jobs": tools.ListJobs(sessionID),
		}, nil

	case "jobs.output":
		sessionID, _ := payload["sessionId"].(string)
		jobID, _ := payload["jobId"].(string)
		if sessionID == "" || jobID == "" {
			return nil, fmt.Errorf("sessionId and jobId are required")
		}
		out, ok := tools.ReadJobOutput(sessionID, jobID)
		if !ok {
			return nil, fmt.Errorf("unknown job %q for this session", jobID)
		}
		return map[string]any{"output": out}, nil

	case "jobs.kill":
		sessionID, _ := payload["sessionId"].(string)
		jobID, _ := payload["jobId"].(string)
		if sessionID == "" || jobID == "" {
			return nil, fmt.Errorf("sessionId and jobId are required")
		}
		ok := tools.KillJob(sessionID, jobID)
		if !ok {
			return nil, fmt.Errorf("unknown job %q for this session", jobID)
		}
		return map[string]any{"killed": jobID}, nil

	case "plugin.list":
		list := []map[string]any{}
		if s.Plugins != nil {
			for _, p := range s.Plugins.ListInfo() {
				list = append(list, pluginInfoValue(p))
			}
		}
		return map[string]any{"plugins": list}, nil

	case "plugin.install":
		if s.Plugins == nil {
			return nil, fmt.Errorf("plugin service unavailable")
		}
		path, _ := payload["path"].(string)
		if path == "" {
			return nil, fmt.Errorf("plugin.install requires a path")
		}
		info, err := s.Plugins.InstallFromPath(ctx, path, s.PluginDir)
		if err != nil {
			return nil, err
		}
		return map[string]any{"plugin": pluginInfoValue(info)}, nil

	case "plugin.uninstall":
		if s.Plugins == nil {
			return nil, fmt.Errorf("plugin service unavailable")
		}
		name, _ := payload["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("plugin.uninstall requires a name")
		}
		if err := s.Plugins.Uninstall(name); err != nil {
			return nil, err
		}
		return map[string]any{"uninstalled": name}, nil

	case "plugin.enable":
		if s.Plugins == nil {
			return nil, fmt.Errorf("plugin service unavailable")
		}
		name, _ := payload["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("plugin.enable requires a name")
		}
		enabled, _ := payload["enabled"].(bool)
		if err := s.Plugins.SetEnabled(name, enabled); err != nil {
			return nil, err
		}
		return map[string]any{"name": name, "enabled": enabled}, nil

	case "git.detect":
		// {} -> {isRepo, root, repoRoot?, head?, detached?, sha?, reason?}
		return s.gitService().Detect(ctx)

	case "git.status":
		// {} -> {branch, oid?, upstream?, ahead, behind, detached?, clean, entries:[...]}
		return s.gitService().Status(ctx)

	case "git.diff":
		// {path?, staged?} -> {patch, truncated?, stats:[{path, additions, deletions, binary?}],
		//                      totalAdditions, totalDeletions}
		g := s.gitService()
		staged, _ := payload["staged"].(bool)
		return g.Diff(ctx, strAny(payload["path"], ""), staged)

	case "git.log":
		// {limit?, offset?} -> {commits:[{hash, abbrev, author, timestamp, subject}]}
		limitF, _ := payload["limit"].(float64)
		offsetF, _ := payload["offset"].(float64)
		return s.gitService().Log(ctx, int(limitF), int(offsetF))

	case "git.branches":
		// {} -> {current?, detached?, sha?, branches:[{name, kind, fullRef, sha,
		//        isHead?, upstream?, ahead?, behind?, gone?}]}
		return s.gitService().Branches(ctx)

	case "git.stage":
		// {paths:[...]} -> {count}
		return s.gitService().Stage(ctx, toStringSlice(payload["paths"]))

	case "git.unstage":
		// {paths:[...]} -> {count}
		return s.gitService().Unstage(ctx, toStringSlice(payload["paths"]))

	case "git.commit":
		// {message} -> {committed, sha}
		message, _ := payload["message"].(string)
		return s.gitService().Commit(ctx, message)

	case "git.discard":
		// {path, confirm} -> {count}; confirm=false is refused with an
		// explanation of the irreversible consequences.
		path, _ := payload["path"].(string)
		confirm, _ := payload["confirm"].(bool)
		return s.gitService().Discard(ctx, path, confirm)

	case "approval.respond":
		// GUI 对 host/permission-request 的一次性决策（allow_once/allow_all/deny/cancel）。
		callID, _ := payload["callId"].(string)
		decisionText, _ := payload["decision"].(string)
		var decision tools.ApprovalDecision
		switch decisionText {
		case "allow_once":
			decision = tools.ApprovalAllowOnce
		case "allow_all":
			// 本会话总是允许：pipeline.go 已定义该决策语义（工具白名单记忆），
			// GUI 审批卡第三主按钮直发此值；缺此分支会被 default 折叠成 cancel。
			decision = tools.ApprovalAllowAll
		case "deny":
			decision = tools.ApprovalDeny
		default:
			decision = tools.ApprovalCancel
		}
		// 取走并注销该在途审批。发送与注销必须发生在同一把 s.mu 临界区内：
		// 它与 askApproval 超时路径的存活检查配对，使双方对"谁拥有裁决权"的
		// 判定原子一致（否则超时侧可能在决策已投递后误判仍在等待）。容量 1
		// 的缓冲通道保证持锁发送不会阻塞。
		s.mu.Lock()
		rec, ok := s.pendingApprovals[callID]
		if ok {
			delete(s.pendingApprovals, callID)
			rec.ch <- decision
		}
		s.mu.Unlock()
		if !ok {
			return map[string]any{"status": "unknown"}, nil
		}
		// 终态广播：所有在途 GUI 弹窗随 host/permission-resolved 关闭；
		// 同一 hub 临界区内撤下重放帧，后续重连的下行不再复活该审批
		// （广播与撤暂存的原子配对见 resolvePermission）。
		outcome := "cancelled"
		switch decision {
		case tools.ApprovalAllowOnce, tools.ApprovalAllowAll:
			outcome = "allowed"
		case tools.ApprovalDeny:
			outcome = "denied"
		}
		s.Hub.resolvePermission(callID, outcome)
		return map[string]any{"status": "ok"}, nil

	case "session.rename":
		// {sessionId, title} -> {ok:true}。用户固定命名：经该会话 actor 落一条
		// session/title 事件（source.kind="user"），与 AutoTitle 的 fallback
		// 快照同构，GUI 依最新一条 title 事件取标题。零存储契约变更。
		sessionID, _ := payload["sessionId"].(string)
		title, _ := payload["title"].(string)
		if sessionID == "" {
			return nil, fmt.Errorf("session.rename requires a sessionId")
		}
		title = strings.TrimSpace(title)
		if title == "" {
			return nil, fmt.Errorf("session.rename requires a non-empty title")
		}
		if len([]rune(title)) > maxRenameTitleRunes {
			return nil, fmt.Errorf("session.rename title too long: %d runes (max %d)",
				len([]rune(title)), maxRenameTitleRunes)
		}
		ag, err := s.ensureLiveAgent(sessionID)
		if err != nil {
			return nil, fmt.Errorf("cannot rename %s: %w", sessionID, err)
		}
		if _, err := ag.EmitEvent(session.EventSessionTitle, map[string]any{
			"title":       title,
			"messageSeqs": []int{},
			"source":      map[string]any{"kind": "user"},
		}); err != nil {
			return nil, fmt.Errorf("append session/title: %w", err)
		}
		return map[string]any{"ok": true}, nil

	case "session.delete":
		// {sessionId} -> {ok:true}。软删除：从 Store 删除 header，停止 live agent，
		// 广播 host/session-deleted 供前端移除列表项。
		sessionID, _ := payload["sessionId"].(string)
		if sessionID == "" {
			return nil, fmt.Errorf("session.delete requires a sessionId")
		}
		if _, ok := s.lookupHeader(sessionID); !ok {
			return nil, fmt.Errorf("session not found: %s", sessionID)
		}
		s.mu.Lock()
		agDel, hasAg := s.agents[sessionID]
		delete(s.agents, sessionID)
		s.mu.Unlock()
		if hasAg && agDel != nil {
			agDel.Stop()
		}
		if deleter, ok := s.Store.(interface{ DeleteSession(string) error }); ok {
			if err := deleter.DeleteSession(sessionID); err != nil {
				return nil, err
			}
		}
		s.Hub.BroadcastHostEvent("host/session-deleted", map[string]any{"id": sessionID})
		return map[string]any{"ok": true}, nil

	default:
		return nil, fmt.Errorf("unknown gateway RPC method: %s", method)
	}
}

// AttachSubagentManager binds the process-level subagent manager and relays
// lifecycle events to host downlinks so the Godot subagent tree can render
// the agent lineage (subagent.started / subagent.finished equivalents).
func (s *Server) AttachSubagentManager(m *subagent.Manager) {
	if m == nil {
		return
	}
	m.SetLifecycleHooks(subagent.LifecycleHooks{
		OnStarted: func(parent, child string) {
			s.Hub.BroadcastHostEvent("host/subagent-started", map[string]any{
				"parentSessionId": parent,
				"childSessionId":  child,
			})
		},
		OnFinished: func(provider, agentID, parent, child, stopReason string, lastAssistant []session.ContentBlock) {
			s.Hub.BroadcastHostEvent("host/subagent-finished", map[string]any{
				"provider":         provider,
				"agentId":          agentID,
				"parentSessionId":  parent,
				"childSessionId":   child,
				"status":           "ok",
				"stopReason":       stopReason,
				"lastAssistantMsg": lastAssistant,
			})
		},
	})
}

// hostVersion returns the injected build version, falling back to a dev tag
// when the host never set one.
func (s *Server) hostVersion() string {
	if s.Version != "" {
		return s.Version
	}
	return "dev"
}

// providerProfile is the resolved view of one provider configuration.
type providerProfile struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Protocol      string         `json:"protocol"`
	BaseURL       string         `json:"baseUrl"`
	Model         string         `json:"model"`
	APIKeyRef     string         `json:"apiKeyRef"`
	KeyConfigured bool           `json:"keyConfigured"`
	KeySource     string         `json:"keySource"`
	KeyWritable   bool           `json:"keyWritable"`
	Raw           map[string]any `json:"-"`
}

// providerState reads the "provider" settings namespace into an active id and a
// map of resolved profiles. It tolerates an unregistered/absent namespace.
func (s *Server) providerState() (string, map[string]providerProfile) {
	active := ""
	profiles := map[string]providerProfile{}
	if s.Settings == nil {
		return active, profiles
	}
	v, err := s.Settings.Get("provider")
	if err != nil {
		return active, profiles
	}
	raw, _ := v.(map[string]any)
	if raw == nil {
		return active, profiles
	}
	if a, ok := raw["active"].(string); ok {
		active = a
	}
	pm, _ := raw["profiles"].(map[string]any)
	if pm == nil {
		return active, profiles
	}
	for id, pv := range pm {
		pmap, _ := pv.(map[string]any)
		if pmap == nil {
			continue
		}
		p := providerProfile{
			ID:        id,
			Name:      strAny(pmap["name"], id),
			Protocol:  strAny(pmap["protocol"], llm.ProtocolOpenAICompletions),
			BaseURL:   strAny(pmap["baseUrl"], llm.DefaultDeepSeekBaseURL),
			Model:     strAny(pmap["model"], llm.DefaultDeepSeekModel),
			APIKeyRef: strAny(pmap["apiKeyRef"], "DEEPSEEK_API_KEY"),
			Raw:       pmap,
		}
		if s.Credentials != nil {
			if info, err := s.Credentials.Describe(p.APIKeyRef); err == nil {
				p.KeyConfigured = info.Configured
				p.KeySource = info.Source
				p.KeyWritable = info.Writable
			}
		}
		profiles[id] = p
	}
	return active, profiles
}

// providerDescribeResp renders the provider.describe payload.
func (s *Server) providerDescribeResp() map[string]any {
	active, profiles := s.providerState()
	usable := false
	for _, p := range profiles {
		if p.ID == active && p.KeyConfigured {
			usable = true
		}
	}
	list := make([]map[string]any, 0, len(profiles))
	for _, p := range profiles {
		list = append(list, map[string]any{
			"id":            p.ID,
			"name":          p.Name,
			"protocol":      p.Protocol,
			"baseUrl":       p.BaseURL,
			"model":         p.Model,
			"apiKeyRef":     p.APIKeyRef,
			"keyConfigured": p.KeyConfigured,
			"keySource":     p.KeySource,
			"keyWritable":   p.KeyWritable,
		})
	}
	return map[string]any{"active": active, "profiles": list, "usable": usable}
}

// setActiveProvider persists the active id and reconfigures the adapter.
func (s *Server) setActiveProvider(id string) (map[string]any, error) {
	if s.Settings != nil {
		if _, err := s.Settings.Mutate("provider", []settings.Op{
			{Op: "set", Path: []string{"active"}, Value: id},
		}); err != nil {
			return nil, err
		}
	}
	if _, err := s.applyProviderConfig(id); err != nil {
		return nil, err
	}
	return s.providerDescribeResp(), nil
}

// applyProviderConfig reconfigures the live adapter from the named profile so
// it takes effect without a restart. The profile is turned into a protocol
// adapter and swapped into the process Router (or replaces LlmAdapter and
// live agents when the adapter is not a Router).
func (s *Server) applyProviderConfig(id string) (map[string]any, error) {
	_, profiles := s.providerState()
	p, ok := profiles[id]
	if !ok {
		return nil, fmt.Errorf("unknown provider profile %q", id)
	}
	protocol := p.Protocol
	if protocol == "" || protocol == "deepseek" {
		protocol = llm.ProtocolOpenAICompletions
	}
	adapter, err := llm.NewProtocolAdapter(llm.ProviderProfile{
		Protocol:       protocol,
		BaseURL:        p.BaseURL,
		Model:          p.Model,
		APIKeyResolver: s.keyResolverFor(p.APIKeyRef),
	})
	if err != nil {
		return nil, err
	}
	if p.Model != "" {
		s.Model = p.Model
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.LlmAdapter.(*llm.Router); ok {
		r.Swap(adapter)
	} else {
		s.LlmAdapter = adapter
		for _, ag := range s.agents {
			if ag != nil {
				ag.LlmAdapter = adapter
			}
		}
	}
	if p.Model != "" {
		for _, ag := range s.agents {
			if ag != nil {
				ag.ModelName = p.Model
			}
		}
	}
	return s.providerDescribeResp(), nil
}

// HydrateRuntime applies the persisted provider profile and selected model so
// the live adapter matches disk (not only the CLI --model flag). Call after
// Settings and Credentials are attached. SeedDefaultProvider is invoked first.
func (s *Server) HydrateRuntime() {
	s.SeedDefaultProvider()
	active, _ := s.providerState()
	if active != "" {
		if _, err := s.applyProviderConfig(active); err != nil {
			// Adapter stays on the process default; picker still lists catalog.
			_ = err
		}
	}
	if s.Settings == nil {
		return
	}
	v, err := s.Settings.Get("general")
	if err != nil {
		return
	}
	raw, _ := v.(map[string]any)
	if raw == nil {
		return
	}
	model, _ := raw["model"].(string)
	if model == "" {
		return
	}
	s.Model = model
	switch a := s.LlmAdapter.(type) {
	case *llm.Router:
		a.SetModel(model)
	case *llm.DeepSeekAdapter:
		a.SetModel(model)
	}
}

// SeedDefaultProvider writes a `deepseek` openai-completions profile when the
// provider namespace has no profiles yet.
func (s *Server) SeedDefaultProvider() {
	if s.Settings == nil {
		return
	}
	_, profiles := s.providerState()
	if len(profiles) > 0 {
		return
	}
	cur := map[string]any{
		"id":        "deepseek",
		"name":      "deepseek",
		"protocol":  llm.ProtocolOpenAICompletions,
		"baseUrl":   llm.DefaultDeepSeekBaseURL,
		"model":     llm.DefaultDeepSeekModel,
		"apiKeyRef": "DEEPSEEK_API_KEY",
	}
	ops := []settings.Op{
		{Op: "set", Path: []string{"profiles", "deepseek"}, Value: cur},
	}
	active, _ := s.providerState()
	if active == "" {
		ops = append(ops, settings.Op{Op: "set", Path: []string{"active"}, Value: "deepseek"})
	}
	_, _ = s.Settings.Mutate("provider", ops)
}

// keyResolverFor binds a credential reference (default DEEPSEEK_API_KEY).
func (s *Server) keyResolverFor(ref string) func() (string, error) {
	if ref == "" {
		ref = "DEEPSEEK_API_KEY"
	}
	return func() (string, error) {
		if s.Credentials == nil {
			return "", nil
		}
		return s.Credentials.ResolveValue(ref)
	}
}

// pluginInfoValue maps plugin.PluginInfo onto a camelCase RPC object.
func pluginInfoValue(p plugin.PluginInfo) map[string]any {
	caps := p.Capabilities
	if caps == nil {
		caps = []string{}
	}
	return map[string]any{
		"name":         p.Name,
		"abiVersion":   p.ABIVersion,
		"status":       p.Status,
		"command":      p.Command,
		"source":       p.Source,
		"capabilities": caps,
		"error":        p.Error,
	}
}

// ensureLiveAgent returns a live actor for sessionID, spawning (or replacing a
// dead map entry) the same way session.resume does. A stored session is never
// 404'd just because the actor is missing.
func (s *Server) ensureLiveAgent(sessionID string) (*agent.Agent, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	s.mu.RLock()
	ag, exists := s.agents[sessionID]
	s.mu.RUnlock()
	if exists && ag.Alive() {
		return ag, nil
	}
	header, ok := s.lookupHeader(sessionID)
	if !ok {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	s.spawnAgent(header)
	s.mu.RLock()
	ag = s.agents[sessionID]
	s.mu.RUnlock()
	if ag == nil || !ag.Alive() {
		return nil, fmt.Errorf("session not found or active: %s", sessionID)
	}
	return ag, nil
}

// keyResolver returns a resolver bound to the current provider profile's
// apiKeyRef (falling back to DEEPSEEK_API_KEY), for fetching models.
func (s *Server) keyResolver() func() (string, error) {
	return func() (string, error) {
		ref := "DEEPSEEK_API_KEY"
		active, profiles := s.providerState()
		if active != "" {
			if p, ok := profiles[active]; ok && p.APIKeyRef != "" {
				ref = p.APIKeyRef
			}
		}
		if s.Credentials == nil {
			return "", nil
		}
		return s.Credentials.ResolveValue(ref)
	}
}

// rpcCtx returns a bounded context for outbound provider calls during an RPC.
func (s *Server) rpcCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	// The timeout is the bound; we intentionally let the caller run to completion.
	_ = cancel
	return ctx
}

func strAny(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// configuredModel returns the active model id for meter/selection purposes.
// s.Model is authoritative and is never clamped to the static catalog, so a
// live provider id stays selected. Empty falls back to the active profile's
// model, then DefaultModels[0].
// normalizeEffort maps a user-supplied effort string to a valid
// reasoning_effort value ("off"|"low"|"high"|"max"); unknown → false.
func normalizeEffort(e string) (string, bool) {
	switch e {
	case "off", "low", "high", "max":
		return e, true
	default:
		return "", false
	}
}

func (s *Server) resolvedContextLimit() (int, string) {
	if limit, ok := s.userContextLimit(); ok {
		return limit, "user"
	}
	model := s.configuredModel()
	for _, m := range llm.DefaultModels {
		if m.ID == model && m.ContextWindow >= 1000 {
			return m.ContextWindow, "model"
		}
	}
	if len(llm.DefaultModels) > 0 && llm.DefaultModels[0].ContextWindow >= 1000 {
		return llm.DefaultModels[0].ContextWindow, "default"
	}
	return 1048576, "default"
}

func (s *Server) resolvedContextLimitTokens() int {
	limit, _ := s.resolvedContextLimit()
	return limit
}

func (s *Server) userContextLimit() (int, bool) {
	if s.Settings == nil {
		return 0, false
	}
	v, err := s.Settings.Get("general")
	if err != nil {
		return 0, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	var n int
	switch x := m["contextLimitTokens"].(type) {
	case int:
		n = x
	case int32:
		n = int(x)
	case int64:
		n = int(x)
	case float64:
		if float64(int(x)) == x {
			n = int(x)
		}
	case json.Number:
		i, err := x.Int64()
		if err == nil {
			n = int(i)
		}
	}
	return n, n >= 1000
}

func numberValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case float32:
		return float64(n), !math.IsNaN(float64(n)) && !math.IsInf(float64(n), 0)
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	default:
		return 0, false
	}
}

func (s *Server) configuredModel() string {
	if s.Model != "" {
		return s.Model
	}
	active, profiles := s.providerState()
	if active != "" {
		if p, ok := profiles[active]; ok && p.Model != "" {
			return p.Model
		}
	}
	if len(llm.DefaultModels) == 0 {
		return llm.DefaultDeepSeekModel
	}
	return llm.DefaultModels[0].ID
}

// modelInfoMaps is the llm.models wire shape: [{id, name, contextWindow, modalities}].
func modelInfoMaps(models []llm.ModelInfo) []map[string]any {
	list := make([]map[string]any, 0, len(models))
	for _, m := range models {
		list = append(list, map[string]any{
			"id":            m.ID,
			"name":          m.Name,
			"contextWindow": m.ContextWindow,
			"modalities":    m.Modalities,
		})
	}
	return list
}

// mergeModelCatalog returns live models first, then any fallback id not already present.
func mergeModelCatalog(live, fallback []llm.ModelInfo) []llm.ModelInfo {
	out := make([]llm.ModelInfo, 0, len(live)+len(fallback))
	seen := make(map[string]struct{}, len(live)+len(fallback))
	for _, src := range [][]llm.ModelInfo{live, fallback} {
		for _, m := range src {
			if m.ID == "" {
				continue
			}
			if _, ok := seen[m.ID]; ok {
				continue
			}
			seen[m.ID] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}

// workspaceList returns the real workspace surface. When a WorkspaceMgr is set,
// it lists the managed workspaces and attaches each one's bounded directory-tree
// scan (for the frontend directory picker). Otherwise it falls back to the
// legacy behavior: the injected Workspaces roots, else the process working
// directory, each reported as a path-only entry.
func (s *Server) workspaceList() []map[string]any {
	if s.WorkspaceMgr != nil {
		list := make([]map[string]any, 0)
		for _, w := range s.WorkspaceMgr.List() {
			entry := map[string]any{
				"id":   w.ID,
				"name": w.Name,
				"path": w.Path,
			}
			if w.Title != "" {
				entry["title"] = w.Title
			}
			if len(w.Sessions) > 0 {
				entry["sessions"] = w.Sessions
			}
			if tree, err := s.WorkspaceMgr.Scan(w.Path, workspace.ScanOptions{}); err == nil {
				entry["tree"] = tree
			}
			list = append(list, entry)
		}
		return list
	}
	paths := s.Workspaces
	if len(paths) == 0 {
		if cwd, err := os.Getwd(); err == nil {
			paths = []string{cwd}
		} else {
			paths = []string{"."}
		}
	}
	list := make([]map[string]any, 0, len(paths))
	for i, p := range paths {
		list = append(list, map[string]any{
			"id":   fmt.Sprintf("ws-%d", i),
			"name": fmt.Sprintf("Workspace %d", i),
			"path": p,
		})
	}
	return list
}

// gitService binds a GitService to the active workspace root, resolving the
// root exactly like workspaceList does: first managed workspace, then the
// injected Workspaces roots, else the process working directory. Constructed
// per call (it is stateless) so workspace changes take effect immediately
// without touching the Server wiring.
func (s *Server) gitService() *tools.GitService {
	root := ""
	if s.WorkspaceMgr != nil {
		if list := s.WorkspaceMgr.List(); len(list) > 0 {
			root = list[0].Path
		}
	}
	if root == "" && len(s.Workspaces) > 0 && s.Workspaces[0] != "" {
		root = s.Workspaces[0]
	}
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	return tools.NewGitService(root)
}

// toStringSlice coerces a []any of strings into a []string (lenient: nil -> empty).
func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// settingsHasDocument reports whether the settings document file exists (or
// the manager can enumerate a namespace with user overrides). The gateway uses
// it for the hasDocument field of settings.describe.
func settingsHasDocument(m *settings.Manager) bool {
	if m == nil {
		return false
	}
	for _, d := range m.Describe() {
		if len(d.User) > 0 {
			return true
		}
	}
	return m.DocumentExists()
}

// spawnAgent starts (or replaces) the in-process actor for header. NewAgent
// already seeds seq from the store so resume appends contiguously. A previous
// !Alive() entry left by session.stop is replaced.
func (s *Server) spawnAgent(header session.SessionHeader) {
	id := header.ID
	ringBuf := storage.NewRingBuffer(512)
	ag := agent.NewAgent(header, ringBuf, nil, s.Store, s.Tools, s.LlmAdapter, "You are DSHX Assistant.", s.configuredModel())
	ag.HookBus = s.HookBus
	ag.Hooks = s.Hooks
	ag.AutoTitle = true
	ag.RequestUser = func(prompt string, options []string) (tools.ApprovalDecision, error) {
		return s.askApproval(id, prompt, options)
	}
	ag.Start()

	sub := ag.Subscribe()
	go func() {
		for env := range sub {
			s.Hub.BroadcastSessionEvent(id, env)
		}
	}()

	s.mu.Lock()
	old := s.agents[id]
	s.agents[id] = ag
	s.mu.Unlock()
	if old != nil && old != ag {
		old.Stop()
	}
}

// lookupHeader finds a session header by id from the store, else a live/dead
// in-memory actor still held in the map.
func (s *Server) lookupHeader(id string) (session.SessionHeader, bool) {
	if s.Store != nil {
		list, err := s.Store.ListSessions()
		if err == nil {
			for _, h := range list {
				if h.ID == id {
					return h, true
				}
			}
		}
	}
	s.mu.RLock()
	ag, ok := s.agents[id]
	s.mu.RUnlock()
	if ok && ag != nil {
		return ag.Header, true
	}
	return session.SessionHeader{}, false
}

// approvalTimeoutNanos bounds how long one permission request waits on the
// GUI (default 60s). Atomic because tests shorten it while long-lived agents
// elsewhere in the suite may still be parked inside askApproval's timer read.
var approvalTimeoutNanos atomic.Int64

func setApprovalTimeout(d time.Duration) { approvalTimeoutNanos.Store(int64(d)) }

// maxRenameTitleRunes bounds session.rename titles (rune-counted so CJK
// titles get the same 200 characters as Latin ones).
const maxRenameTitleRunes = 200

func approvalTimeoutDelay() time.Duration {
	if n := approvalTimeoutNanos.Load(); n > 0 {
		return time.Duration(n)
	}
	return 60 * time.Second
}

// resolvePermission closes out one approval on the host downlink: it first
// broadcasts host/permission-resolved {callId, outcome} so every connected GUI
// dismisses its modal, then unstages the replay frame inside the same hub
// critical section. The order is load-bearing: a downlink reconnecting between
// broadcast and unstage receives the resolved frame live and must NOT also get
// the dead request replayed — holding h.mu across both makes the snapshot
// (registerMux/registerHost) see either "request staged, no resolution" or
// "resolution sent, request gone", never a resurrected prompt.
//
// outcome vocabulary aligns with approval.respond decisions:
// "allowed" | "denied" | "cancelled" | "timeout".
func (h *DownlinkHub) resolvePermission(callID, outcome string) {
	h.mu.Lock()
	frame, err := encodeHostEvent("host/permission-resolved", map[string]any{
		"callId":  callID,
		"outcome": outcome,
	})
	if err == nil {
		for conn := range h.hostClients {
			conn.enqueue(frame)
		}
	}
	for i, f := range h.stagedFrames {
		if f.id == callID {
			h.stagedFrames = append(h.stagedFrames[:i], h.stagedFrames[i+1:]...)
			break
		}
	}
	h.mu.Unlock()
}

// askApproval issues one host-level permission request and waits for the GUI's
// decision (upstream approval/request -> requestPermission bridge, mirrored by
// the ACP permission-request path). The timeout returns ApprovalCancel so a
// stuck modal cannot deadlock the agent loop.
//
// 在途审批随下行重连重放：帧材料（method + payload + 稳定 callId）在广播前
// 暂存进 hub，任何之后接入的 /api/events/host|mux 连接都会按原 callId 收到
// 重放；决策或超时后撤下，避免复活已决弹窗。
func (s *Server) askApproval(sessionID, prompt string, options []string) (tools.ApprovalDecision, error) {
	callID := fmt.Sprintf("approval-%d", time.Now().UnixNano())
	ch := make(chan tools.ApprovalDecision, 1)
	s.mu.Lock()
	s.pendingApprovals[callID] = &pendingApproval{sessionID: sessionID, ch: ch}
	s.mu.Unlock()

	optionList := make([]map[string]string, 0, len(options))
	for _, opt := range options {
		label := opt
		switch opt {
		case "allow_once":
			label = "Allow once"
		case "allow_all":
			label = "Always allow this session"
		case "deny":
			label = "Reject"
		case "cancel":
			label = "Cancel"
		}
		optionList = append(optionList, map[string]string{"optionId": opt, "name": label})
	}

	payload := map[string]any{
		"callId":    callID,
		"sessionId": sessionID,
		"prompt":    prompt,
		"options":   optionList,
	}
	if frame, err := encodeHostEvent("host/permission-request", payload); err == nil {
		s.Hub.stageReplay(callID, frame)
		s.Hub.broadcastHostData(frame, false)
	} else {
		s.Hub.BroadcastHostEvent("host/permission-request", payload)
	}

	select {
	case decision := <-ch:
		// 决策路径：approval.respond 已广播 host/permission-resolved 并撤下
		// 重放帧，这里只消费结果。
		return decision, nil
	case <-time.After(approvalTimeoutDelay()):
		// 超时兜底：与 approval.respond 的取走-投递在同一把 s.mu 下配对，
		// 谁先取走注册项谁拥有裁决权。若决策已抢先投递（select 双就绪时
		// 可能随机选中本分支），直接消费缓冲中的决策且不广播 timeout——
		// 一个 callId 的终态帧必须恰好一条。
		s.mu.Lock()
		_, stillPending := s.pendingApprovals[callID]
		delete(s.pendingApprovals, callID)
		s.mu.Unlock()
		if !stillPending {
			return <-ch, nil
		}
		s.Hub.resolvePermission(callID, "timeout")
		return tools.ApprovalCancel, nil
	}
}

// cancelSessionApprovals unblocks every in-flight askApproval for sessionID
// (GUI Stop / session.stop). Missing or already-resolved prompts are no-ops.
func (s *Server) cancelSessionApprovals(sessionID string) {
	if sessionID == "" {
		return
	}
	type pair struct {
		id string
		ch chan tools.ApprovalDecision
	}
	var found []pair
	s.mu.Lock()
	for id, rec := range s.pendingApprovals {
		if rec != nil && rec.sessionID == sessionID {
			found = append(found, pair{id: id, ch: rec.ch})
			delete(s.pendingApprovals, id)
		}
	}
	s.mu.Unlock()
	for _, p := range found {
		select {
		case p.ch <- tools.ApprovalCancel:
		default:
		}
		s.Hub.resolvePermission(p.id, "cancelled")
	}
}
