package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	// Continuable 标记该子代理会话是否仍可接收后续用户消息（多轮 continue）。
	// invoke_subagent 单轮完成后置 true，前提是底层 Agent 循环仍存活。
	Continuable bool `json:"continuable"`
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

// SubagentNode 是谱系树节点：携带子代理元数据与其后代子树。
// 供 list_descendants / ListDescendants 以树形结构完整展示子代理谱系。
type SubagentNode struct {
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	ParentID  string          `json:"parentId"`
	CreatedAt int64           `json:"createdAt"`
	Children  []*SubagentNode `json:"children,omitempty"`
}

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
				// 子代理 actorLoop 在单轮结束后仍存活（阻塞于 nextTurnChan），
				// 可继续接收后续消息；据此标记可续写。Stop() 会取消其循环。
				Continuable: true,
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
				ID          string `json:"id"`
				Role        string `json:"role"`
				CreatedAt   int64  `json:"createdAt"`
				Continuable bool   `json:"continuable"`
			}
			var list []Item
			for _, info := range m.subagents {
				if info.ParentID == ctx.SessionID {
					list = append(list, Item{
						ID:          info.ID,
						Role:        info.Role,
						CreatedAt:   info.CreatedAt,
						Continuable: info.Continuable,
					})
				}
			}
			return list, nil
		},
	})

	// 3. list_descendants — 树状枚举当前会话的完整子代理谱系。
	r.Register(tools.ToolDefinition{
		Name:        "list_descendants",
		Description: "List the full descendant subagent tree spawned from the current session (recursive, for lineage visualization).",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"depth": { "type": "integer", "description": "Optional maximum recursion depth; -1 or omitted means unlimited" }
			}
		}`),
		Execute: func(ctx tools.ToolExecutionContext, argsJSON string) (any, error) {
			depth := -1
			if len(argsJSON) > 0 {
				var args struct {
					Depth int `json:"depth"`
				}
				_ = json.Unmarshal([]byte(argsJSON), &args)
				if args.Depth > 0 {
					depth = args.Depth
				}
			}
			root := m.ListDescendants(ctx.SessionID, depth)
			if root == nil {
				return []*SubagentNode{}, nil
			}
			return []*SubagentNode{root}, nil
		},
	})
}

// listDescendants 构建以 originSessionID 为根的子代理谱系树，深度限制为
// depth（<0 表示不限）。返回 nil 表示该会话下无任何子代理。
// 它是公开方法 ListDescendants 的可测内部实现。
func (m *Manager) listDescendants(originSessionID string, depth int) *SubagentNode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.listDescendantsLocked(originSessionID, depth)
}

// listDescendantsLocked 在持有读锁的前提下构建子树（内部专用，不自行加锁）。
// depth 表示 origin 之下允许展开的后代层数：0 仅直接子层；depth<0 表示不限。
func (m *Manager) listDescendantsLocked(originSessionID string, depth int) *SubagentNode {
	var children []*SubagentInfo
	for _, info := range m.subagents {
		if info.ParentID == originSessionID {
			children = append(children, info)
		}
	}
	if len(children) == 0 {
		return nil
	}
	sort.Slice(children, func(i, j int) bool { return children[i].CreatedAt < children[j].CreatedAt })

	root := &SubagentNode{ID: originSessionID, Children: []*SubagentNode{}}
	for _, c := range children {
		// 直接子层之下还允许 levels 层：subagentNodeLocked 会据此决定是否继续展开。
		root.Children = append(root.Children, m.subagentNodeLocked(c.ID, depth))
	}
	return root
}

// subagentNodeLocked 递归构建单个子代理节点及其（可限深的）后代。
// depth 为该节点自身以下还可展开的层数：0 停止；<0 不限。
func (m *Manager) subagentNodeLocked(id string, depth int) *SubagentNode {
	info := m.subagents[id]
	if info == nil {
		return nil
	}
	n := &SubagentNode{
		ID:        info.ID,
		Role:      info.Role,
		ParentID:  info.ParentID,
		CreatedAt: info.CreatedAt,
	}
	if depth != 0 {
		for _, c := range m.subagents {
			if c.ParentID == id {
				n.Children = append(n.Children, m.subagentNodeLocked(c.ID, depth-1))
			}
		}
		if len(n.Children) > 0 {
			sort.Slice(n.Children, func(i, j int) bool { return n.Children[i].CreatedAt < n.Children[j].CreatedAt })
		}
	}
	return n
}

// ListDescendants 是公开的谱系树枚举入口，返回以 originSessionID 为根的
// 完整后代子树（含所有层级）。depth < 0 表示不限深度；depth >= 0 限制递归
// 层数。无子代理时返回 nil。
func (m *Manager) ListDescendants(originSessionID string, depth int) *SubagentNode {
	return m.listDescendants(originSessionID, depth)
}

// ContinueSubagent 向已存在（continuable）的子代理会话投递一条后续用户消息，
// 使子代理在其既有会话上下文中继续多轮（continuation）。返回子代理新一轮
// 文本输出；若该子代理不存在、已不可续写或底层 Agent 已停止则返回错误。
// 该实现采用基于 RingBuffer 的尽力重读，适合无持久化 store 的宿主；失败时
// 以不可续写兜底，不影响既有 invoke_subagent 生命周期。
func (m *Manager) ContinueSubagent(subID, prompt string) (string, error) {
	m.mu.RLock()
	info := m.subagents[subID]
	m.mu.RUnlock()
	if info == nil {
		return "", fmt.Errorf("subagent %s not found", subID)
	}
	if !info.Continuable {
		return "", fmt.Errorf("subagent %s is not continuable", subID)
	}
	ca := info.Agent
	if ca == nil {
		return "", fmt.Errorf("subagent %s has no agent", subID)
	}
	// 注意：Agent.IsRunning() 仅表示「是否在轮中」，单轮结束后为 false，
	// 但 actorLoop 仍存活并阻塞等待下一条消息，因此不以此作为续写可用性的
	// 判据。续写通过向既有会话投递新 turn 实现；若底层循环已被 Stop() 取消，
	// 事件流将保持静默并在超时后兜底返回，不会破坏既有生命周期。

	eventsChan := ca.Subscribe()
	ca.PostUserMessage(session.UserMessagePayload{
		ID:   fmt.Sprintf("cont-msg-%d", time.Now().UnixNano()),
		Role: "user",
		Content: []session.ContentBlock{
			{Type: "text", Text: prompt},
		},
		Source: session.MessageSource{Kind: "user"},
	})

	var finalResponse string
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
				break collectLoop
			}
		case <-timeout:
			ca.Stop()
			return fmt.Sprintf("Subagent '%s' timed out. Partial output: %s", info.Role, finalResponse), nil
		}
	}
	if finalResponse == "" {
		finalResponse = fmt.Sprintf("Subagent '%s' continued with no text output.", info.Role)
	}
	return finalResponse, nil
}

// ContinueSubagentByRole 按 role 匹配的「父会话下最近创建的可续子代理」续写，
// 供上游 continue 语义（role + prompt）使用。未找到匹配子代理时返回错误。
func (m *Manager) ContinueSubagentByRole(parentSessionID, role, prompt string) (string, error) {
	m.mu.RLock()
	var best *SubagentInfo
	for _, info := range m.subagents {
		if info.ParentID == parentSessionID && info.Role == role && info.Continuable {
			if best == nil || info.CreatedAt > best.CreatedAt {
				best = info
			}
		}
	}
	m.mu.RUnlock()
	if best == nil {
		return "", fmt.Errorf("no continuable subagent with role %q under %s", role, parentSessionID)
	}
	return m.ContinueSubagent(best.ID, prompt)
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
