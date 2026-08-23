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
	"dsh-go/pkg/llm"
	"dsh-go/pkg/session"
	"dsh-go/pkg/storage"
	"dsh-go/pkg/subagent"
	"dsh-go/pkg/tools"
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
	agents     map[string]*agent.Agent
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

// workspaceList returns the real workspace root(s) for the jobs/file panels.
// It surfaces the injected Workspaces roots; when none were configured it
// reports the process working directory so clients always get an absolute,
// usable path instead of a hardcoded stub.
func (s *Server) workspaceList() []map[string]any {
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
