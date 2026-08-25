package subagent

import (
	"context"
	"encoding/json"
	"errors"
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
	// DelegationDepth 是该子代理的委托深度（父深度 + 1，上限 MaxDepth）。
	// 对齐上游 lineage stamping（child-agent.ts stampAgentLineage）。
	DelegationDepth int `json:"delegationDepth"`
	// Continuable 标记该子代理会话是否仍可接收后续用户消息（多轮 continue）。
	// invoke_subagent 单轮完成后保持 true，前提是底层 Agent 循环仍存活；
	// Stop()/超时/取消路径在 settle 时复位为 false。
	Continuable bool `json:"continuable"`
}

// alive 报告该条目是否应继续列出/可续：底层循环未取消即视为存活。
// Agent 为 nil 的手工注入条目按「状态未知」保守视为存活（兼容既有
// 测试与外部注入路径）；真实 spawn 的子代理始终携带非 nil Agent，
// 其停止（超时/取消 Stop）后由本判据驱动复位与回收。
func (info *SubagentInfo) alive() bool {
	return info.Agent == nil || info.Agent.Alive()
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

const (
	// DefaultModelName 是未注入 ModelGetter 且父请求模型未知时的兜底路由，
	// 保持既有行为不变（上游 tool-subagent agentOptions.model 的缺省语义）。
	DefaultModelName = "deepseek-chat"

	// MaxDepth 是注册表强制的最大委托深度：depth 0 拒绝委托，默认 3
	// （上游 tool-subagent/src/index.ts maxDepth z.natural().default(3)；
	// child-agent.ts SubagentDepthError 语义）。
	MaxDepth = 3

	// DefaultTimeout 是单个子代理 turn 的默认等待上限，可用 WithTimeout 覆盖。
	DefaultTimeout = 60 * time.Second

	// UsageFooterKey 是工具结果文本尾部结构化 JSON 行的标记键：
	// 父会话据此把子代理 token 用量并入计量。
	UsageFooterKey = "subagentUsage"
)

// DepthError 是超过最大委托深度的结构化错误（对照上游 SubagentDepthError：
// attemptedDepth / maxDepth 字段与消息文案一一对应）。调用方可用
// errors.As(e, &DepthError{}) 识别并提取机器可读字段。
type DepthError struct {
	AttemptedDepth int `json:"attemptedDepth"`
	MaxDepth       int `json:"maxDepth"`
}

func (e *DepthError) Error() string {
	return fmt.Sprintf("subagent depth %d exceeds maxDepth %d", e.AttemptedDepth, e.MaxDepth)
}

// resolveChildDepth 由父深度解析子代理深度并强制注册表上限
// （上游 resolveChildDepth(child-agent.ts:48-57)：超限抛 SubagentDepthError）。
func resolveChildDepth(parentDepth int) (int, error) {
	child := parentDepth + 1
	if child > MaxDepth {
		return child, &DepthError{AttemptedDepth: child, MaxDepth: MaxDepth}
	}
	return child, nil
}

// SubagentNode 是谱系树节点：携带子代理元数据与其后代子树。
// 供 list_descendants / ListDescendants 以树形结构完整展示子代理谱系。
type SubagentNode struct {
	ID              string          `json:"id"`
	Role            string          `json:"role"`
	ParentID        string          `json:"parentId"`
	CreatedAt       int64           `json:"createdAt"`
	DelegationDepth int             `json:"delegationDepth,omitempty"`
	Children        []*SubagentNode `json:"children,omitempty"`
}

// Manager orchestrates hierarchical subagent execution.
type Manager struct {
	toolReg    *tools.ToolRegistry
	llmAdapter llm.LlmAdapter
	subagents  map[string]*SubagentInfo
	hooks      LifecycleHooks
	mu         sync.RWMutex

	// store 与父会话共享的持久化 sink：非 nil 时透传给每个子 Agent，
	// 使子会话事件落库（此前子会话只在内存）。
	store agent.PersistSink
	// timeout 是单个子代理 turn 的等待上限；0 取 DefaultTimeout（60s）。
	timeout time.Duration
	// modelGetter 返回父请求当前模型（每次 spawn 时查询，模型可热切换）；
	// nil 时回落父请求快照中的模型，再回落 DefaultModelName。
	modelGetter func() string
}

// Option 配置 Manager 的可选依赖（保持 NewManager 两参签名兼容既有调用方，
// 如 pkg/gateway/sdk_server.go 的进程内缺省管理器）。
type Option func(*Manager)

// WithStore 注入与会话存储一致的持久化 sink（gateway.SessionStore /
// storage.SqliteGatewayStore 等均满足 agent.PersistSink）。子会话事件随之
// 落库；传 nil 与不传等价（内存模式，ACP stdio / 测试路径）。
func WithStore(store agent.PersistSink) Option {
	return func(m *Manager) { m.store = store }
}

// WithTimeout 覆盖子代理单轮等待上限（<=0 视为 DefaultTimeout）。
func WithTimeout(d time.Duration) Option {
	return func(m *Manager) {
		if d <= 0 {
			d = DefaultTimeout
		}
		m.timeout = d
	}
}

// WithModelGetter 注入「当前模型」查询函数；每次 spawn 子代理时调用，
// 使子代理路由继承父请求模型（模型热切换即时生效）。
func WithModelGetter(getter func() string) Option {
	return func(m *Manager) { m.modelGetter = getter }
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

// effectiveTimeout 返回配置化超时（未配置回落默认 60s）。
func (m *Manager) effectiveTimeout() time.Duration {
	if m.timeout > 0 {
		return m.timeout
	}
	return DefaultTimeout
}

// currentModel 返回注入 getter 的查询结果（可为空串，表示未知）；
// 空串时由调用方依次回落父请求快照模型、DefaultModelName。
func (m *Manager) currentModel() string {
	if m.modelGetter != nil {
		return m.modelGetter()
	}
	return ""
}

// headerLister 是可选的会话头枚举能力：gateway.SessionStore 及各存储后端
// 均实现。用于恢复场景——进程重启后注册表为空，父若是持久化的子代理，
// 其真实 delegationDepth 只存在于存储的会话头中。
type headerLister interface {
	ListSessions() ([]session.SessionHeader, error)
}

// parentHeader 解析调用方父会话的路由与委托元数据：
//   - 模型：共享 store 重放最新 request/header 快照（热切换后的真实路由）；
//     无快照时退回注册表中同 ID 条目（父本身是子代理）的 Agent.ModelName。
//   - 深度：优先注册表条目（进程内父子链与上游 resolveChildDepth 的
//     内存 parent 对象一致）；重启后经 headerLister 读持久化会话头兜底。
func (m *Manager) parentHeader(sessionID string) (model string, depth int) {
	m.mu.RLock()
	if info := m.subagents[sessionID]; info != nil && info.Agent != nil {
		model = info.Agent.ModelName
		depth = info.Agent.Header.DelegationDepth
	}
	m.mu.RUnlock()

	if m.store == nil {
		return model, depth
	}
	if events, err := m.store.GetEvents(sessionID, 0); err == nil {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Type != session.EventRequestHeader {
				continue
			}
			var payload session.RequestHeaderPayload
			if json.Unmarshal(events[i].Data, &payload) != nil {
				continue
			}
			if name := payload.Header.Config.Model; name != "" {
				model = name
				break
			}
		}
	}
	if depth == 0 {
		if lister, ok := m.store.(headerLister); ok {
			if headers, err := lister.ListSessions(); err == nil {
				for _, h := range headers {
					if h.ID == sessionID && h.DelegationDepth > depth {
						depth = h.DelegationDepth
						break
					}
				}
			}
		}
	}
	return model, depth
}

// NewManager creates a new subagent manager.
// opts 可选注入共享持久化 sink（WithStore）、超时（WithTimeout）、
// 当前模型 getter（WithModelGetter）。
func NewManager(toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, opts ...Option) *Manager {
	m := &Manager{
		toolReg:    toolReg,
		llmAdapter: adapter,
		subagents:  make(map[string]*SubagentInfo),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
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
				"prompt": { "type": "string", "description": "Specific actionable instruction for the subagent" },
				"model": { "type": "string", "description": "Optional model override; defaults to the parent's current model" }
			},
			"required": ["role", "prompt"]
		}`),
		Execute: func(ctx tools.ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Role   string `json:"role"`
				Prompt string `json:"prompt"`
				Model  string `json:"model,omitempty"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}

			// 委托深度：从父会话 Header 解析 parentDepth 后 +1，注册表强制
			// 上限 MaxDepth（上游 resolveChildDepth + SubagentDepthError）。
			parentModel, parentDepth := m.parentHeader(ctx.SessionID)
			childDepth, err := resolveChildDepth(parentDepth)
			if err != nil {
				var de *DepthError
				if errors.As(err, &de) {
					// 结构化错误体随错误文案一起返回（ExecutePipeline 把
					// err 渲染为 isErr 工具失败文本；workflow_tool 视为
					// spawn 失败），机器可读字段经 errors.As 不丢失。
					payload, _ := json.Marshal(map[string]any{
						"error":          "SubagentDepthError",
						"attemptedDepth": de.AttemptedDepth,
						"maxDepth":       de.MaxDepth,
						"message":        de.Error(),
					})
					return fmt.Sprintf("Error: %s\n%s", de.Error(), string(payload)), de
				}
				return nil, err
			}

			subID := fmt.Sprintf("%s/sub-%d", ctx.SessionID, time.Now().UnixNano()%100000)
			header := session.SessionHeader{
				ID:              subID,
				ParentSession:   ctx.SessionID,
				CreatedAt:       time.Now().UnixMilli(),
				Cwd:             ctx.Cwd,
				Origin:          "subagent",
				DelegationDepth: childDepth,
			}

			ringBuf := storage.NewRingBuffer(256)
			// 模型路由：invoke 参数显式覆盖 > 父请求当前模型（getter 每次
			// spawn 时查询，支持热切换；无 getter 时用注册表/store 已知的
			// 父请求模型）> 默认模型。System 保持默认文案。
			childModel := args.Model
			if childModel == "" {
				childModel = m.currentModel()
			}
			if childModel == "" {
				childModel = parentModel
			}
			if childModel == "" {
				childModel = DefaultModelName
			}
			childAgent := agent.NewAgent(
				header,
				ringBuf,
				nil,
				m.store,
				m.toolReg,
				m.llmAdapter,
				fmt.Sprintf("You are a specialized subagent with role: %s.", args.Role),
				childModel,
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
				ID:              subID,
				Role:            args.Role,
				ParentID:        ctx.SessionID,
				Agent:           childAgent,
				CreatedAt:       time.Now().UnixMilli(),
				DelegationDepth: childDepth,
				// 子代理 actorLoop 在单轮结束后仍存活（阻塞于 nextTurnChan），
				// 可继续接收后续消息；据此标记可续写。Stop() 会取消其循环，
				// settle 阶段按 Agent.Alive() 复位该标记。
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

			// Wait for subagent response or turn/end。超时可配（默认 60s）；
			// 三条退出路径（turn/end、超时、取消）都走统一收尾：Stop 存活
			// 循环、复位 Continuable、按需回收条目、附结构化用量行返回。
			var finalResponse string
			var usage session.TokenUsage
			stopReason := "error"
			exitKind := "" // "" 正常收束 | "timeout" | "canceled"
			// 可停止定时器：提前收束时释放，避免每个子代理泄漏一个满时长
			// 定时器（time.After 在 GC 前持有 timer 资源）。
			timer := time.NewTimer(m.effectiveTimeout())
			defer timer.Stop()

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
						// Token 归集：累加子代理每步 usage（此前只拼文本丢弃）。
						mergeUsage(&usage, msg.Usage)
					}
					if env.Type == session.EventTurnEnd {
						var te session.TurnEndPayload
						_ = json.Unmarshal(env.Data, &te)
						stopReason = mapTurnKind(te.Reason.Kind)
						break collectLoop
					}
				case <-timer.C:
					// 超时属 aborted（区别于 error 兜底）：中止存活循环。
					stopReason = "aborted"
					exitKind = "timeout"
					childAgent.Stop()
					break collectLoop
				case <-execCtx.Done():
					stopReason = "aborted"
					exitKind = "canceled"
					childAgent.Stop()
					break collectLoop
				}
			}

			m.settleSubagent(info, stopReason)

			switch {
			case exitKind == "timeout":
				return fmt.Sprintf("Subagent '%s' aborted: timed out after %s. Partial output: %s\n%s",
					args.Role, m.effectiveTimeout(), finalResponse, usageFooter(usage)), nil
			case exitKind == "canceled":
				return fmt.Sprintf("Subagent execution canceled.\n%s", usageFooter(usage)), nil
			}

			if finalResponse == "" {
				finalResponse = fmt.Sprintf("Subagent '%s' completed successfully.", args.Role)
			} else if usage.InputTokens+usage.OutputTokens > 0 {
				finalResponse += "\n" + usageFooter(usage)
			}
			lastAssistant := []session.ContentBlock{{Type: "text", Text: finalResponse}}
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
			m.mu.Lock()
			defer m.mu.Unlock()

			type Item struct {
				ID              string `json:"id"`
				Role            string `json:"role"`
				CreatedAt       int64  `json:"createdAt"`
				Continuable     bool   `json:"continuable"`
				DelegationDepth int    `json:"delegationDepth,omitempty"`
			}
			var list []Item
			for id, info := range m.subagents {
				// 回收判据：底层 actor 循环已停止（Alive()==false）的条目
				// 直接移除——注册表不再只增不删。
				if !info.alive() {
					delete(m.subagents, id)
					continue
				}
				if info.ParentID == ctx.SessionID {
					list = append(list, Item{
						ID:              info.ID,
						Role:            info.Role,
						CreatedAt:       info.CreatedAt,
						Continuable:     info.Continuable,
						DelegationDepth: info.DelegationDepth,
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

// usageFooter 渲染随工具结果文本尾部返回给父会话的结构化用量行：
// {"subagentUsage":{"inputTokens":N,"outputTokens":N,...}}。
func usageFooter(u session.TokenUsage) string {
	b, err := json.Marshal(map[string]session.TokenUsage{UsageFooterKey: u})
	if err != nil {
		return ""
	}
	return string(b)
}

// mergeUsage 把 src 的各计数累加进 dst（invoke / continue 归集复用；
// src 为 nil 时无操作）。包级函数：Go 不允许为本包外类型定义方法。
func mergeUsage(dst *session.TokenUsage, src *session.TokenUsage) {
	if src == nil {
		return
	}
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
	dst.ReasoningTokens += src.ReasoningTokens
}

// settleSubagent 统一收尾一个子代理条目：按底层循环存活状态复位 Continuable
// （超时/取消路径已 Stop 循环 → 复位 false），并把不可再续写的死条目从注册表
// 移除（map 不再只增不删）。存活条目保留以支持后续 continue 轮。
func (m *Manager) settleSubagent(info *SubagentInfo, stopReason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info.Continuable = info.Continuable && info.alive()
	if !info.Continuable {
		delete(m.subagents, info.ID)
	}
}

// listDescendants 构建以 originSessionID 为根的子代理谱系树，深度限制为
// depth（<0 表示不限）。返回 nil 表示该会话下无任何存活子代理。
// 它是公开方法 ListDescendants 的可测内部实现。
func (m *Manager) listDescendants(originSessionID string, depth int) *SubagentNode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.listDescendantsLocked(originSessionID, depth)
}

// listDescendantsLocked 在持有读锁的前提下构建子树（内部专用，不自行加锁）。
// depth 表示 origin 之下允许展开的后代层数：0 仅直接子层；depth<0 表示不限。
// 已停止（Alive()==false）的子代理不出现在谱系树中。
func (m *Manager) listDescendantsLocked(originSessionID string, depth int) *SubagentNode {
	var children []*SubagentInfo
	for _, info := range m.subagents {
		if info.ParentID == originSessionID && info.alive() {
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
		ID:              info.ID,
		Role:            info.Role,
		ParentID:        info.ParentID,
		CreatedAt:       info.CreatedAt,
		DelegationDepth: info.DelegationDepth,
	}
	if depth != 0 {
		for _, c := range m.subagents {
			if c.ParentID == id && c.alive() {
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
// 层数。无存活子代理时返回 nil。
func (m *Manager) ListDescendants(originSessionID string, depth int) *SubagentNode {
	return m.listDescendants(originSessionID, depth)
}

// ContinueSubagent 向已存在（continuable）的子代理会话投递一条后续用户消息，
// 使子代理在其既有会话上下文中继续多轮（continuation）。返回子代理新一轮
// 文本输出（含结构化用量行）；若该子代理不存在、已不可续写或底层 Agent 已
// 停止则返回错误。续写同样受配置化超时约束，并在收尾时复位/回收状态。
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
	var usage session.TokenUsage
	timedOut := false
	timer := time.NewTimer(m.effectiveTimeout())
	defer timer.Stop()
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
				mergeUsage(&usage, msg.Usage)
			}
			if env.Type == session.EventTurnEnd {
				break collectLoop
			}
		case <-timer.C:
			// 超时区分于静默兜底：中止循环并标注 aborted 文案。
			timedOut = true
			ca.Stop()
			break collectLoop
		}
	}
	// 续写轮同样收尾：底层循环已被 Stop()（如本轮超时路径）则复位
	// Continuable 并回收条目；正常完成的存活循环保持可续。
	m.settleSubagent(info, map[bool]string{true: "aborted", false: "completed"}[timedOut])
	if timedOut {
		return fmt.Sprintf("Subagent '%s' aborted: timed out after %s. Partial output: %s\n%s",
			info.Role, m.effectiveTimeout(), finalResponse, usageFooter(usage)), nil
	}
	if finalResponse == "" {
		finalResponse = fmt.Sprintf("Subagent '%s' continued with no text output.", info.Role)
	} else if usage.InputTokens+usage.OutputTokens > 0 {
		finalResponse += "\n" + usageFooter(usage)
	}
	return finalResponse, nil
}

// ContinueSubagentByRole 按 role 匹配的「父会话下最近创建的可续子代理」续写，
// 供上游 continue 语义（role + prompt）使用。未找到匹配子代理时返回错误；
// 已停止（Alive()==false）的条目不参与匹配。
func (m *Manager) ContinueSubagentByRole(parentSessionID, role, prompt string) (string, error) {
	m.mu.RLock()
	var best *SubagentInfo
	for _, info := range m.subagents {
		if info.ParentID == parentSessionID && info.Role == role && info.Continuable && info.alive() {
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
	case "max-tokens":
		return "max-tokens"
	default:
		return "error"
	}
}
