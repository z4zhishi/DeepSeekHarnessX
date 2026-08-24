package gateway

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
	"dsh-go/pkg/storage"
	"dsh-go/pkg/subagent"
	"dsh-go/pkg/tools"
)

// SDK line-level JSON-RPC 2.0 transport over stdio, ported from
// `CK/packages/sdk/protocol/src/transport.ts` and
// `CK/packages/sdk/server/src/server.ts`.
//
// Methods: initialize (returns serverInfo.name "deepseek-harness-sdk-runtime"),
// session/prompt (lazy session creation, returns messageId), shutdown (flush
// then exit 0). Notifications: session.event, session.status, subagent.started,
// subagent.finished. Unknown methods -> -32601; handler failures -> -32603;
// malformed lines are ignored.

// SDKServer implements the DeepSeek Harness SDK JSON-RPC runtime over stdio.
type SDKServer struct {
	reader    *bufio.Reader
	writer    io.Writer
	store     SessionStore
	tools     *tools.ToolRegistry
	adapter   llm.LlmAdapter
	model     string
	exit      func(code int)
	subagents *subagent.Manager

	mu           sync.Mutex
	sessions     map[string]*agent.Agent
	shuttingDown bool
	shutdownOnce sync.Once
}

// NewSDKServer creates the SDK JSON-RPC server over the given streams.
func NewSDKServer(store SessionStore, tools *tools.ToolRegistry, adapter llm.LlmAdapter, model string) *SDKServer {
	return &SDKServer{
		reader:    bufio.NewReader(os.Stdin),
		writer:    os.Stdout,
		store:     store,
		tools:     tools,
		adapter:   adapter,
		model:     model,
		exit:      os.Exit,
		sessions:  make(map[string]*agent.Agent),
		subagents: subagent.NewManager(tools, adapter),
	}
}

// NewSDKServerWithIO 构建带注入流的服务器（测试用）。
func NewSDKServerWithIO(r io.Reader, w io.Writer, store SessionStore, tools *tools.ToolRegistry, adapter llm.LlmAdapter, model string) *SDKServer {
	return &SDKServer{
		reader:    bufio.NewReader(r),
		writer:    w,
		store:     store,
		tools:     tools,
		adapter:   adapter,
		model:     model,
		exit:      func(int) {},
		sessions:  make(map[string]*agent.Agent),
		subagents: subagent.NewManager(tools, adapter),
	}
}

// AttachSubagentManager 绑定进程级 subagent 管理器并挂上 SDK 生命周期通知。
// 由 main 流程注入（复用已注册 invoke_subagent 工具的同一管理器），
// 否则 SDK 内部构造的独立管理器不会接收子代理调用事件。
func (s *SDKServer) AttachSubagentManager(m *subagent.Manager) {
	if m == nil {
		return
	}
	s.subagents = m
	m.SetLifecycleHooks(subagent.LifecycleHooks{
		OnStarted: func(parent, child string) {
			s.Notify("subagent.started", map[string]any{
				"parentSessionId": parent,
				"childSessionId":  child,
			})
		},
		OnFinished: func(provider, agentID, parent, child, stopReason string, lastAssistant []session.ContentBlock) {
			status := "ok"
			if stopReason != "completed" {
				status = "error"
			}
			payload := map[string]any{
				"provider":        provider,
				"agentId":         agentID,
				"parentSessionId": parent,
				"childSessionId":  child,
				"status":          status,
				"stopReason":      stopReason,
			}
			if len(lastAssistant) > 0 {
				payload["lastAssistantMessage"] = lastAssistant
			}
			s.Notify("subagent.finished", payload)
		},
	})
}

// Serve reads newline-delimited JSON-RPC frames until EOF or shutdown.
func (s *SDKServer) Serve(ctx context.Context) error {
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
		var frame struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      any             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			continue // malformed lines are ignored
		}
		if frame.ID == nil || frame.Method == "" {
			// Notifications (method only) or responses (id only); this server
			// owns requests only, so both are ignored.
			continue
		}
		s.handleRequest(frame.ID, frame.Method, frame.Params)
	}
}

func (s *SDKServer) handleRequest(id any, method string, params json.RawMessage) {
	result, err := s.dispatch(method, params)
	if err != nil {
		code := -32603
		if isUnknownSDKMethod(err) {
			code = -32601
		}
		s.write(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    code,
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

func isUnknownSDKMethod(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "unknown DeepSeek Harness SDK runtime method")
}

func (s *SDKServer) dispatch(method string, params json.RawMessage) (any, error) {
	var p map[string]any
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	switch method {
	case "initialize":
		return s.initialize(p)
	case "session/prompt":
		return s.prompt(p)
	case "shutdown":
		return s.shutdown()
	default:
		return nil, fmt.Errorf("unknown DeepSeek Harness SDK runtime method: %s", method)
	}
}

func (s *SDKServer) initialize(p map[string]any) (any, error) {
	// cwd/provider/model are recorded on every SDK-created session; maxTokens
	// is inherited by agents.
	return map[string]any{
		"serverInfo": map[string]string{
			"name":    "deepseek-harness-sdk-runtime",
			"version": "1.0.0",
		},
	}, nil
}

// getOrCreateSession lazily creates the agent+session pair for an unknown id.
func (s *SDKServer) getOrCreateSession(sessionID string) (*agent.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ag, ok := s.sessions[sessionID]; ok {
		return ag, nil
	}
	header := session.SessionHeader{
		ID:        sessionID,
		CreatedAt: time.Now().UnixMilli(),
		Cwd:       ".",
		Origin:    "sdk",
	}
	if s.store != nil {
		_ = s.store.PutSession(&header)
	}
	ag := agent.NewAgent(header, storage.NewRingBuffer(512), nil, s.store, s.tools, s.adapter, "You are DSHX Assistant.", s.model)
	ag.Start()
	s.sessions[sessionID] = ag
	// Pipe the agent's live event stream to the client as session.event
	// notifications; status transitions are derived from the lifecycle events.
	sub := ag.Subscribe()
	go s.forwardAgentEvents(sessionID, sub)
	return ag, nil
}

// forwardAgentEvents relays one agent's events as session.event notifications
// and derives session.status transitions (idle|running) from turn lifecycle.
func (s *SDKServer) forwardAgentEvents(sessionID string, sub chan *session.SessionEnvelope) {
	for env := range sub {
		s.Notify("session.event", map[string]any{
			"sessionId": sessionID,
			"event":     env,
		})
		switch env.Type {
		case session.EventTurnStart:
			s.Notify("session.status", map[string]any{
				"sessionId": sessionID,
				"status":    "running",
			})
		case session.EventTurnEnd:
			s.Notify("session.status", map[string]any{
				"sessionId": sessionID,
				"status":    "idle",
			})
		}
	}
}

func (s *SDKServer) prompt(p map[string]any) (any, error) {
	sessionID, _ := p["sessionId"].(string)
	if sessionID == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	contentBlocks, _ := p["contentBlocks"].([]any)
	var blocks []session.ContentBlock
	for _, raw := range contentBlocks {
		data, _ := json.Marshal(raw)
		var block session.ContentBlock
		if err := json.Unmarshal(data, &block); err != nil {
			return nil, fmt.Errorf("invalid content block: %w", err)
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("contentBlocks must not be empty")
	}
	ag, err := s.getOrCreateSession(sessionID)
	if err != nil {
		return nil, err
	}
	messageID := fmt.Sprintf("sdk-msg-%d", time.Now().UnixNano())
	ag.PostUserMessage(session.UserMessagePayload{
		ID:      messageID,
		Role:    "user",
		Content: blocks,
		Source:  session.MessageSource{Kind: "user"},
	})
	return map[string]any{"messageId": messageID}, nil
}

func (s *SDKServer) shutdown() (any, error) {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		for _, ag := range s.sessions {
			ag.Stop()
		}
		s.sessions = map[string]*agent.Agent{}
		s.shuttingDown = true
		s.mu.Unlock()
	})
	return map[string]any{}, nil
}

// write serializes one frame with a trailing newline.
func (s *SDKServer) write(frame map[string]any) {
	data, _ := json.Marshal(frame)
	s.mu.Lock()
	_, _ = s.writer.Write(append(data, '\n'))
	s.mu.Unlock()
}

// Notify sends a JSON-RPC notification to the client.
func (s *SDKServer) Notify(method string, params any) {
	s.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}
