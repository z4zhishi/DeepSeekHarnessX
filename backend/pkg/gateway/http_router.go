package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
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
	// Model is the configured default model id (e.g. "deepseek-chat"). It is
	// reported by llm.models and seeds the token meter's context window for
	// session.context. Empty falls back to the catalog's default model.
	Model  string
	agents map[string]*agent.Agent
	// subagents is the process-level subagent manager whose lifecycle events
	// are relayed to the host downlink (Godot subagent tree).
	subagents *subagent.Manager
	// pendingApprovals tracks in-flight one-shot permission requests keyed by callId;
	// the Godot GUI answers via the approval.respond RPC (mirrors ACP bridge).
	pendingApprovals map[string]chan tools.ApprovalDecision
	mu               sync.RWMutex
}

// NewServer creates a new API server.
func NewServer(store SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter) *Server {
	return NewServerWithVersion(store, toolReg, adapter, "dev")
}

// NewServerWithVersion creates a new API server with an explicit host version.
func NewServerWithVersion(store SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, version string) *Server {
	return &Server{
		Store:            store,
		Hub:              NewDownlinkHub(),
		Tools:            toolReg,
		LlmAdapter:       adapter,
		Version:          version,
		agents:           make(map[string]*agent.Agent),
		pendingApprovals: make(map[string]chan tools.ApprovalDecision),
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

	// CSRF/Host 信任栅栏：网关只服务 loopback 客户端
	return s.trustGuard(mux)
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "http://127.0.0.1")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	method := strings.TrimPrefix(r.URL.Path, "/api/")

	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
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

func (s *Server) dispatch(ctx any, method string, payload map[string]any) (any, error) {
	switch method {
	case "host.describe":
		return map[string]any{
			"ready":       true,
			"engine":      "dsh-go-godot",
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

		// Instantiate agent actor
		ringBuf := storage.NewRingBuffer(512)
		ag := agent.NewAgent(header, ringBuf, nil, s.Store, s.Tools, s.LlmAdapter, "You are DSH Assistant.", "deepseek-chat")
		// CC-style hooks runtime: the shared registry's event bus + loaded
		// hooks drive the agent's four dispatch intercept points (best-effort;
		// nil leaves them inert).
		ag.HookBus = s.HookBus
		ag.Hooks = s.Hooks
		// 会话标题：首条人工消息后的首个完成回合生成一次 log-only
		// `session/title` 快照（first-prompt 模式；失败静默降级为确定性回退）。
		ag.AutoTitle = true
		// 审批瀑布：需权限的工具调用经 host 下行通知 GUI，GUI 通过
		// approval.respond RPC 回填决策（与 ACP permission-request 桥同构）。
		ag.RequestUser = func(prompt string, options []string) (tools.ApprovalDecision, error) {
			return s.askApproval(id, prompt, options)
		}
		ag.Start()

		// Pipe agent events to downlink hub
		sub := ag.Subscribe()
		go func() {
			for env := range sub {
				s.Hub.BroadcastSessionEvent(id, env)
			}
		}()

		s.mu.Lock()
		s.agents[id] = ag
		s.mu.Unlock()

		s.Hub.BroadcastHostEvent("host/session-added", header)

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
		metrics, err := (llm.Meter{ContextLimit: llm.ContextLimitForModel(s.configuredModel())}).Measure(events)
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

	case "session.prompt":
		sessionID, _ := payload["sessionId"].(string)
		text, _ := payload["text"].(string)

		s.mu.RLock()
		ag, ok := s.agents[sessionID]
		s.mu.RUnlock()

		if !ok {
			return nil, fmt.Errorf("session not found or active: %s", sessionID)
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
		// {ns, ops:[{op:"set"|"unset", path, value?}]} -> {revision}
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
		rev, err := s.Settings.Mutate(ns, ops)
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
		// { } -> {models:[{id, name, contextWindow, modalities}], active}
		// Model catalog for the frontend model picker (aligned with upstream
		// DEFAULT_MODELS). active reports the currently selected model id so the
		// picker can render the live selection.
		models := llm.DefaultModels
		list := make([]map[string]any, 0, len(models))
		for _, m := range models {
			list = append(list, map[string]any{
				"id":            m.ID,
				"name":          m.Name,
				"contextWindow": m.ContextWindow,
				"modalities":    m.Modalities,
			})
		}
		return map[string]any{"models": list, "selected": s.configuredModel()}, nil

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

	case "approval.respond":
		// GUI 对 host/permission-request 的一次性决策（allow_once/deny/cancel）。
		callID, _ := payload["callId"].(string)
		outcome, _ := payload["decision"].(string)
		s.mu.Lock()
		ch, ok := s.pendingApprovals[callID]
		if ok {
			delete(s.pendingApprovals, callID)
		}
		s.mu.Unlock()
		if !ok {
			return map[string]any{"status": "unknown"}, nil
		}
		switch outcome {
		case "allow_once":
			ch <- tools.ApprovalAllowOnce
		case "deny":
			ch <- tools.ApprovalDeny
		default:
			ch <- tools.ApprovalCancel
		}
		return map[string]any{"status": "ok"}, nil

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

// configuredModel returns the active model id for meter/selection purposes. The
// host's Model (default "deepseek-chat") is authoritative; a malformed value
// falls back to the catalog's first (default chat) model.
func (s *Server) configuredModel() string {
	if s.Model == "" {
		return llm.DefaultModels[0].ID
	}
	for _, m := range llm.DefaultModels {
		if m.ID == s.Model {
			return m.ID
		}
	}
	return llm.DefaultModels[0].ID
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

// askApproval issues one host-level permission request and waits for the GUI's
// decision (upstream approval/request -> requestPermission bridge, mirrored by
// the ACP permission-request path). A 60s timeout returns ApprovalCancel so a
// stuck modal cannot deadlock the agent loop.
func (s *Server) askApproval(sessionID, prompt string, options []string) (tools.ApprovalDecision, error) {
	callID := fmt.Sprintf("approval-%d", time.Now().UnixNano())
	ch := make(chan tools.ApprovalDecision, 1)
	s.mu.Lock()
	s.pendingApprovals[callID] = ch
	s.mu.Unlock()

	optionList := make([]map[string]string, 0, len(options))
	for _, opt := range options {
		label := opt
		switch opt {
		case "allow_once":
			label = "Allow once"
		case "deny":
			label = "Reject"
		case "cancel":
			label = "Cancel"
		}
		optionList = append(optionList, map[string]string{"optionId": opt, "name": label})
	}
	s.Hub.BroadcastHostEvent("host/permission-request", map[string]any{
		"callId":    callID,
		"sessionId": sessionID,
		"prompt":    prompt,
		"options":   optionList,
	})

	select {
	case decision := <-ch:
		return decision, nil
	case <-time.After(60 * time.Second):
		s.mu.Lock()
		delete(s.pendingApprovals, callID)
		s.mu.Unlock()
		return tools.ApprovalCancel, nil
	}
}
