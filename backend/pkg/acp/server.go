package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"dsh-go/pkg/agent"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/session"
	"dsh-go/pkg/tools"
)

// Server implements the automation-only Agent Client Protocol over stdio,
// ported from `CK/packages/acp/acp/src/index.ts`. The bridge exposes fresh
// harness sessions to trusted programmatic clients: prompt text, committed
// assistant text, cancellation, and one-shot permission decisions.

// StopReason is the ACP terminal reason vocabulary.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopMaxTokens StopReason = "max_tokens"
	StopCancelled StopReason = "cancelled"
)

// Server implements the ACP stdio server.
type Server struct {
	reader     *bufio.Reader
	writer     io.Writer
	tools      *tools.ToolRegistry
	llmAdapter llm.LlmAdapter
	sessions   map[string]*agent.Agent
	subs       map[string]chan *session.SessionEnvelope
	// pendingApprovals tracks in-flight one-shot permission requests keyed by
	// callId; the client answers via permission/request.
	pendingApprovals map[string]chan tools.ApprovalDecision
	mu               sync.Mutex
	closed           bool
}

// NewServer creates a new ACP stdio server.
func NewServer(toolReg *tools.ToolRegistry, adapter llm.LlmAdapter) *Server {
	return &Server{
		reader:           bufio.NewReader(os.Stdin),
		writer:           os.Stdout,
		tools:            toolReg,
		llmAdapter:       adapter,
		sessions:         make(map[string]*agent.Agent),
		subs:             make(map[string]chan *session.SessionEnvelope),
		pendingApprovals: make(map[string]chan tools.ApprovalDecision),
	}
}

// NewServerWithIO builds a server over injected streams (tests).
func NewServerWithIO(r io.Reader, w io.Writer, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter) *Server {
	s := NewServer(toolReg, adapter)
	s.reader = bufio.NewReader(r)
	s.writer = w
	return s
}

// Serve reads JSON-RPC 2.0 lines from stdin and writes responses/notifications
// to stdout until EOF or ctx cancellation.
func (s *Server) Serve(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		line, err := s.reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(line) == 0 {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      any             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if req.ID == nil || req.Method == "" {
			continue // notifications / responses
		}
		s.handleMethod(req.ID, req.Method, req.Params)
	}
}

// handleMethod dispatches one request and writes its response frame.
func (s *Server) handleMethod(id any, method string, params json.RawMessage) {
	result, err := s.dispatch(method, params)
	if err != nil {
		s.write(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32603,
				"message": err.Error(),
			},
		})
		return
	}
	s.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

// dispatch routes one ACP method.
func (s *Server) dispatch(method string, params json.RawMessage) (any, error) {
	var p map[string]any
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "1.0",
			"serverInfo": map[string]string{
				"name":    "dsh-go-acp",
				"version": "1.0.0",
			},
			"capabilities": map[string]any{
				"imagePrompt": false,
				"tools":       true,
			},
		}, nil

	case "session/new":
		cwd, _ := p["cwd"].(string)
		sessionID := fmt.Sprintf("acp-%d", time.Now().UnixNano())
		header := session.SessionHeader{
			ID:        sessionID,
			CreatedAt: time.Now().UnixMilli(),
			Cwd:       cwd,
			Origin:    "acp",
		}
		ag := agent.NewAgent(header, nil, nil, nil, s.tools, s.llmAdapter, "You are ACP automation assistant.", "deepseek-chat")
		ag.RequestUser = func(prompt string, options []string) (tools.ApprovalDecision, error) {
			return s.askApproval(sessionID, prompt, options)
		}
		ag.Start()
		s.mu.Lock()
		s.sessions[sessionID] = ag
		sub := ag.Subscribe()
		s.subs[sessionID] = sub
		s.mu.Unlock()
		go s.relayEvents(sessionID, sub)
		return map[string]string{"sessionId": sessionID}, nil

	case "session/prompt":
		sessionID, _ := p["sessionId"].(string)
		promptText, _ := p["prompt"].(string)
		ag, err := s.requireSession(sessionID)
		if err != nil {
			return nil, err
		}
		ag.PostUserMessage(session.UserMessagePayload{
			ID:   fmt.Sprintf("acp-msg-%d", time.Now().UnixNano()),
			Role: "user",
			Content: []session.ContentBlock{
				{Type: "text", Text: promptText},
			},
			Source: session.MessageSource{Kind: "user"},
		})
		return map[string]bool{"admitted": true}, nil

	case "session/cancel":
		sessionID, _ := p["sessionId"].(string)
		ag, err := s.requireSession(sessionID)
		if err != nil {
			return nil, err
		}
		ag.Stop()
		return map[string]bool{"cancelled": true}, nil

	case "permission/request":
		// The client answers a previously-issued approval request with its
		// one-shot decision (allow-once / reject-once).
		callID, _ := p["callId"].(string)
		outcome, _ := p["outcome"].(map[string]any)
		optionID, _ := outcome["optionId"].(string)
		s.mu.Lock()
		ch, ok := s.pendingApprovals[callID]
		if ok {
			delete(s.pendingApprovals, callID)
		}
		s.mu.Unlock()
		if !ok {
			return map[string]any{"status": "unknown"}, nil
		}
		switch optionID {
		case "allow-once":
			ch <- tools.ApprovalAllowOnce
		case "reject-once":
			ch <- tools.ApprovalDeny
		default:
			ch <- tools.ApprovalCancel
		}
		return map[string]any{"status": "ok"}, nil

	default:
		return nil, fmt.Errorf("unknown ACP method: %s", method)
	}
}

// requireSession returns the session agent or an error.
func (s *Server) requireSession(sessionID string) (*agent.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ag, ok := s.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	return ag, nil
}

// relayEvents forwards one session's events as ACP `session/update`
// notifications, mirroring the upstream bridge's session/event projection.
func (s *Server) relayEvents(sessionID string, sub chan *session.SessionEnvelope) {
	for env := range sub {
		// session/update carries one protocol update: the committed
		// assistant text (text blocks) or lifecycle state.
		switch env.Type {
		case session.EventAssistantMessage:
			var msg session.AssistantMessagePayload
			if err := json.Unmarshal(env.Data, &msg); err != nil {
				continue
			}
			for _, block := range msg.Message.Content {
				if block.Type != "text" || block.Text == "" {
					continue
				}
				s.write(map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": sessionID,
						"update": map[string]any{
							"kind": "text",
							"text": block.Text,
						},
					},
				})
			}
		case session.EventTurnEnd:
			var payload session.TurnEndPayload
			if err := json.Unmarshal(env.Data, &payload); err != nil {
				continue
			}
			reason := turnEndToStopReason(payload.Reason)
			s.notifyStop(sessionID, reason)
		}
	}
}

// notifyStop emits the terminal stop notification for a session.
func (s *Server) notifyStop(sessionID string, reason StopReason) {
	s.notify(sessionID, map[string]any{
		"kind":   "stop",
		"reason": reason,
	})
}

// notify sends one session/update notification (contains transport failure).
func (s *Server) notify(sessionID string, update map[string]any) {
	s.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sessionID,
			"update":    update,
		},
	})
}

// turnEndToStopReason maps a harness turn ending to ACP's terminal reason
// vocabulary (upstream codec.ts).
func turnEndToStopReason(reason session.TurnEndReason) StopReason {
	switch reason.Kind {
	case "completed":
		return StopReason("end_turn")
	case "max-tokens":
		return StopReason("max_tokens")
	case "interrupted":
		return StopReason("cancelled")
	default:
		// aborted/blocked/error report ordinary quiescence.
		return StopReason("end_turn")
	}
}

// write serializes one frame with a trailing newline.
func (s *Server) write(frame map[string]any) {
	data, _ := json.Marshal(frame)
	s.mu.Lock()
	_, _ = s.writer.Write(append(data, '\n'))
	s.mu.Unlock()
}

var _ = strings.TrimSpace

// askApproval issues one ACP permission request and waits for the client's
// one-shot decision (upstream approval/request -> requestPermission bridge).
func (s *Server) askApproval(sessionID, prompt string, options []string) (tools.ApprovalDecision, error) {
	callID := fmt.Sprintf("approval-%d", time.Now().UnixNano())
	ch := make(chan tools.ApprovalDecision, 1)
	s.mu.Lock()
	s.pendingApprovals[callID] = ch
	s.mu.Unlock()

	s.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sessionID,
			"update": map[string]any{
				"kind":   "permission-request",
				"callId": callID,
				"prompt": prompt,
				"options": []map[string]string{
					{"optionId": "allow-once", "name": "Allow once"},
					{"optionId": "reject-once", "name": "Reject"},
				},
			},
		},
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
