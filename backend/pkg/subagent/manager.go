package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"dsh-go/pkg/agent"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/session"
	"dsh-go/pkg/storage"
	"dsh-go/pkg/tools"
)

// SubagentInfo holds metadata of a running child agent.
type SubagentInfo struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	ParentID  string `json:"parentId"`
	Agent     *agent.Agent
	CreatedAt int64 `json:"createdAt"`
}

// LifecycleHooks 是子代理生命周期回调（SDK subagent.started/finished 通知依据）。
type LifecycleHooks struct {
	// OnStarted 在子代理会话创建后调用。
	OnStarted func(parentSessionID, childSessionID string)
	// OnFinished 在子代理运行结束（含超时/取消）时调用；
	// lastAssistant 为空表示子代理无输出。
	OnFinished func(provider, agentID, parentSessionID, childSessionID, stopReason string, lastAssistant []session.ContentBlock)
}

// SubagentProviderName 对齐上游 SDK 默认 provider 路由名。
const SubagentProviderName = "deepseek-official"

// Manager orchestrates hierarchical subagent execution.
type Manager struct {
	toolReg    *tools.ToolRegistry
	llmAdapter llm.LlmAdapter
	subagents  map[string]*SubagentInfo
	hooks      LifecycleHooks
	mu         sync.RWMutex
}

// SetLifecycleHooks 注入生命周期回调（幂等，可随时替换）。
func (m *Manager) SetLifecycleHooks(h LifecycleHooks) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks = h
}

func (m *Manager) notifyStarted(parent, child string) {
	m.mu.RLock()
	h := m.hooks
	m.mu.RUnlock()
	if h.OnStarted != nil {
		h.OnStarted(parent, child)
	}
}

func (m *Manager) notifyFinished(parent, child, stopReason string, lastAssistant []session.ContentBlock) {
	m.mu.RLock()
	h := m.hooks
	m.mu.RUnlock()
	if h.OnFinished != nil {
		h.OnFinished(SubagentProviderName, child, parent, child, stopReason, lastAssistant)
	}
}

// NewManager creates a new subagent manager.
func NewManager(toolReg *tools.ToolRegistry, adapter llm.LlmAdapter) *Manager {
	return &Manager{
		toolReg:    toolReg,
		llmAdapter: adapter,
		subagents:  make(map[string]*SubagentInfo),
	}
}

// RegisterSubagentTools registers subagent invocation tools on the tool registry.
func (m *Manager) RegisterSubagentTools(r *tools.ToolRegistry) {
	// 1. invoke_subagent
	r.Register(tools.ToolDefinition{
		Name:        "invoke_subagent",
		Description: "Spawn a child subagent to perform an isolated delegated task in the background.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"role": { "type": "string", "description": "Role title of the subagent (e.g. Code Reviewer, Researcher)" },
				"prompt": { "type": "string", "description": "Specific actionable instruction for the subagent" }
			},
			"required": ["role", "prompt"]
		}`),
		Execute: func(ctx tools.ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Role   string `json:"role"`
				Prompt string `json:"prompt"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}

			subID := fmt.Sprintf("%s/sub-%d", ctx.SessionID, time.Now().UnixNano()%100000)
			header := session.SessionHeader{
				ID:              subID,
				ParentSession:   ctx.SessionID,
				CreatedAt:       time.Now().UnixMilli(),
				Cwd:             ctx.Cwd,
				Origin:          "subagent",
				DelegationDepth: 1,
			}

			ringBuf := storage.NewRingBuffer(256)
			childAgent := agent.NewAgent(
				header,
				ringBuf,
				nil,
				nil,
				m.toolReg,
				m.llmAdapter,
				fmt.Sprintf("You are a specialized subagent with role: %s.", args.Role),
				"deepseek-chat",
			)

			// 容忍调用方未注入 context（如直接执行工具的测试路径）：
			// nil context 视为不可取消。
			execCtx := ctx.Context
			if execCtx == nil {
				execCtx = context.Background()
			}
			eventsChan := childAgent.Subscribe()
			childAgent.Start()
			m.notifyStarted(ctx.SessionID, subID)

			info := &SubagentInfo{
				ID:        subID,
				Role:      args.Role,
				ParentID:  ctx.SessionID,
				Agent:     childAgent,
				CreatedAt: time.Now().UnixMilli(),
			}

			m.mu.Lock()
			m.subagents[subID] = info
			m.mu.Unlock()

			// Post initial prompt to subagent
			childAgent.PostUserMessage(session.UserMessagePayload{
				ID:   fmt.Sprintf("sub-msg-%d", time.Now().UnixNano()),
				Role: "user",
				Content: []session.ContentBlock{
					{Type: "text", Text: args.Prompt},
				},
				Source: session.MessageSource{Kind: "user"},
			})

			// Wait for subagent response or turn/end
			var finalResponse string
			stopReason := "error"
			timeout := time.After(60 * time.Second)

		collectLoop:
			for {
				select {
				case env, ok := <-eventsChan:
					if !ok {
						break collectLoop
					}
					if env.Type == session.EventAssistantMessage {
						var msg session.AssistantMessagePayload
						_ = json.Unmarshal(env.Data, &msg)
						for _, b := range msg.Message.Content {
							if b.Type == "text" {
								finalResponse += b.Text
							}
						}
					}
					if env.Type == session.EventTurnEnd {
						var te session.TurnEndPayload
						_ = json.Unmarshal(env.Data, &te)
						stopReason = mapTurnKind(te.Reason.Kind)
						break collectLoop
					}
				case <-timeout:
					stopReason = "aborted"
					childAgent.Stop()
					return fmt.Sprintf("Subagent '%s' timed out. Partial output: %s", args.Role, finalResponse), nil
				case <-execCtx.Done():
					stopReason = "aborted"
					childAgent.Stop()
					return "Subagent execution canceled.", nil
				}
			}

			if finalResponse == "" {
				finalResponse = fmt.Sprintf("Subagent '%s' completed successfully.", args.Role)
			}
			var lastAssistant []session.ContentBlock
			if finalResponse != "" {
				lastAssistant = []session.ContentBlock{{Type: "text", Text: finalResponse}}
			}
			m.notifyFinished(ctx.SessionID, subID, stopReason, lastAssistant)
			return finalResponse, nil
		},
	})

	// 2. list_subagents
	r.Register(tools.ToolDefinition{
		Name:           "list_subagents",
		Description:    "List all active child subagents and their statuses.",
		ParametersJSON: json.RawMessage(`{"type": "object", "properties": {}}`),
		Execute: func(ctx tools.ToolExecutionContext, argsJSON string) (any, error) {
			m.mu.RLock()
			defer m.mu.RUnlock()

			type Item struct {
				ID        string `json:"id"`
				Role      string `json:"role"`
				CreatedAt int64  `json:"createdAt"`
			}
			var list []Item
			for _, info := range m.subagents {
				if info.ParentID == ctx.SessionID {
					list = append(list, Item{
						ID:        info.ID,
						Role:      info.Role,
						CreatedAt: info.CreatedAt,
					})
				}
			}
			return list, nil
		},
	})
}

// mapTurnKind 将会话 turn-end kind 映射为 SDK subagent stopReason 词表
// （completed/aborted/error/max-tokens/refusal；未知按 error 兜底）。
func mapTurnKind(kind string) string {
	switch kind {
	case "completed":
		return "completed"
	case "aborted", "interrupted":
		return "aborted"
	case "error":
		return "error"
	default:
		return "error"
	}
}
