package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Standard event types in the DSH event sourcing log.
//
// This is the full vocabulary of `CK/packages/core/session/src/known-event-types.ts`
// (48 types, generated from SessionEventMap). Persistence read paths refuse to
// interpret an unknown type unless the envelope carries `ignorable: true`.
const (
	EventAgentPresetSelected     = "agent-preset/selected"
	EventAgentInboxSpliced       = "agent/inbox/spliced"
	EventApprovalAsked           = "approval/asked"
	EventApprovalDecided         = "approval/decided"
	EventApprovalPolicy          = "approval/policy"
	EventAssistantChunk          = "assistant/chunk"
	EventAssistantMessage        = "assistant/message"
	EventCommandDone             = "command/done"
	EventCommandRun              = "command/run"
	EventCompactionEnd           = "compaction/end"
	EventCompactionPrune         = "compaction/prune"
	EventCompactionStart         = "compaction/start"
	EventCompactionSummary       = "compaction/summary"
	EventFeedbackRecord          = "feedback/record"
	EventGoalChange              = "goal/change"
	EventHookInvoked             = "hook/invoked"
	EventHookResult              = "hook/result"
	EventLlmRetry                = "llm/retry"
	EventLlmRetryStarted         = "llm/retry-started"
	EventPermissionPreset        = "permission/preset"
	EventPlanMode                = "plan/mode"
	EventRequestContext          = "request/context"
	EventRequestHeader           = "request/header"
	EventSandboxMode             = "sandbox/mode"
	EventScheduleChange          = "schedule/change"
	EventSessionEndSeed          = "session/end-seed"
	EventSessionTitle            = "session/title"
	EventSessionTitleLLMRequest  = "session/title-llm-request"
	EventStepEnd                 = "step/end"
	EventStepStart               = "step/start"
	EventSubagentDescriptor      = "subagent/descriptor"
	EventTeamMember              = "team/member"
	EventTeamMessageDelivered    = "team/message/delivered"
	EventTeamMessageQueued       = "team/message/queued"
	EventTeamTask                = "team/task"
	EventTodoWrite               = "todo/write"
	EventToolWorkflowAgentEnd    = "tool-workflow/agent-end"
	EventToolWorkflowAgentStart  = "tool-workflow/agent-start"
	EventToolWorkflowRunEnd      = "tool-workflow/run-end"
	EventToolWorkflowRunStart    = "tool-workflow/run-start"
	EventToolCall                = "tool/call"
	EventToolCodeDispatch        = "tool/code-dispatch"
	EventToolCodeDispatchStart   = "tool/code-dispatch-start"
	EventToolResult              = "tool/result"
	EventTurnEnd                 = "turn/end"
	EventTurnStart               = "turn/start"
	EventUserMessage             = "user/message"
	EventWebDeepseekSearchLLMReq = "web/deepseek-search-llm-request"
)

// SurfaceEventTypes are the event types whose events produce LLM messages and
// are eligible to appear on the ordered surface. Only these may carry
// SurfaceOp / SourceEventSeqs — mirroring upstream `SurfaceEventType`.
var SurfaceEventTypes = map[string]bool{
	EventUserMessage:      true,
	EventAssistantMessage: true,
	EventToolResult:       true,
}

// SurfaceOp describes how a session event entered the ordered surface. Only
// valid on surface-eligible events. `AppendSurfaceOp` is the normal tail path;
// a replace shadows surface nodes from start (inclusive) through end
// (inclusive) with this node (used by compaction).
//
// The wire representation is exactly the upstream `SurfaceOp` union: the bare
// string `"append"` or the object `{"op":"replace","start":n,"end":n}`.
// `IsAppend` is the reliable discriminator: any non-append op is a positional
// replacement.
type SurfaceOp struct {
	Op    string
	Start int
	End   int
}

// AppendSurfaceOp is the canonical tail-append operation. JSON-marshals to the
// bare string `"append"`, exactly as upstream `surfaceOp: 'append'` serializes.
var AppendSurfaceOp = SurfaceOp{Op: "append"}

// MarshalJSON emits the upstream union shape: `"append"` for the append marker,
// `{"op":"replace","start":n,"end":n}` for a positional replacement.
func (s SurfaceOp) MarshalJSON() ([]byte, error) {
	if s.Op == "append" {
		return []byte(`"append"`), nil
	}
	if s.Op == "replace" {
		var buf bytes.Buffer
		buf.WriteString(`{"op":"replace","start":`)
		buf.WriteString(strconv.Itoa(s.Start))
		buf.WriteString(`,"end":`)
		buf.WriteString(strconv.Itoa(s.End))
		buf.WriteString(`}`)
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("invalid surface op %q", s.Op)
}

// UnmarshalJSON accepts both upstream union members: the bare string "append"
// and the positional-replacement object.
func (s *SurfaceOp) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		if value != "append" {
			return fmt.Errorf("invalid surface op %q", value)
		}
		*s = AppendSurfaceOp
		return nil
	}
	var obj struct {
		Op    string `json:"op"`
		Start int    `json:"start"`
		End   int    `json:"end"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	if obj.Op != "replace" {
		return fmt.Errorf("invalid surface op %q", obj.Op)
	}
	*s = SurfaceOp{Op: "replace", Start: obj.Start, End: obj.End}
	return nil
}

// IsAppend reports whether the op is the tail-append marker. An empty SurfaceOp
// (zero value) is NOT a valid surface op — callers must only pass surface ops
// built by the constructors below.
func (s SurfaceOp) IsAppend() bool { return s.Op == "append" }

// ReplaceSurfaceOp builds a positional replacement op (start..end inclusive).
func ReplaceSurfaceOp(start, end int) SurfaceOp {
	return SurfaceOp{Op: "replace", Start: start, End: end}
}

// IsSurfaceEligibleType reports whether an event type can join the
// model-visible surface (upstream `isSurfaceEligibleType`).
func IsSurfaceEligibleType(eventType string) bool {
	return SurfaceEventTypes[eventType]
}

// IsSurfaceEvent reports whether the event is a surface event: it is both
// surface-eligible AND carries a surfaceOp marker (upstream `isSurfaceEvent`).
func (env *SessionEnvelope) IsSurfaceEvent() bool {
	if env == nil || !IsSurfaceEligibleType(env.Type) {
		return false
	}
	return env.SurfaceOp != nil && (env.SurfaceOp.IsAppend() || env.SurfaceOp.Op == "replace")
}

// IsAppendSurfaceEvent reports whether the event appended to the surface tail
// (upstream `isAppendSurfaceEvent`). Replacement copies stay model-only.
func (env *SessionEnvelope) IsAppendSurfaceEvent() bool {
	return env != nil && env.IsSurfaceEvent() && env.SurfaceOp.IsAppend()
}

// IsReplacementSurfaceEvent reports whether the event is a surface replacement
// (upstream `isReplacementSurfaceEvent`).
func (env *SessionEnvelope) IsReplacementSurfaceEvent() bool {
	return env != nil && env.IsSurfaceEvent() && !env.SurfaceOp.IsAppend()
}

// SessionEnvelope represents the canonical envelope wrapping every persistent event.
type SessionEnvelope struct {
	Seq             int             `json:"seq"`
	Time            int64           `json:"time"`
	Type            string          `json:"type"`
	Data            json.RawMessage `json:"data"`
	SourceEventSeqs []int           `json:"sourceEventSeqs,omitempty"`
	SurfaceOp       *SurfaceOp      `json:"surfaceOp,omitempty"`
	Ignorable       bool            `json:"ignorable,omitempty"`
}

// SessionHeader stores metadata for a session (persisted as SQLite session row or Line 1 of JSONL).
type SessionHeader struct {
	Version         int    `json:"version"`
	ID              string `json:"id"`
	CreatedAt       int64  `json:"createdAt"`
	Cwd             string `json:"cwd,omitempty"`
	ParentSession   string `json:"parentSession,omitempty"`
	SeedLength      int    `json:"seedLength,omitempty"`
	Origin          string `json:"origin,omitempty"`
	DelegationDepth int    `json:"delegationDepth,omitempty"`
	AgentPreset     string `json:"agentPreset,omitempty"`
}

// NewEnvelope creates a new typed envelope with the current unix millisecond timestamp.
func NewEnvelope(seq int, eventType string, payload any) (*SessionEnvelope, error) {
	dataBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal event payload: %w", err)
	}
	return &SessionEnvelope{
		Seq:  seq,
		Time: time.Now().UnixMilli(),
		Type: eventType,
		Data: dataBytes,
	}, nil
}

// ContentBlock is a union representing elements in user/assistant messages.
type ContentBlock struct {
	Type         string         `json:"type"`                   // "text" | "reasoning" | "image" | "tool-call" | "tool-result"
	Text         string         `json:"text,omitempty"`         // for "text" & "reasoning"
	AttachmentID string         `json:"attachmentId,omitempty"` // for "image"
	MimeType     string         `json:"mimeType,omitempty"`     // for "image"
	ID           string         `json:"id,omitempty"`           // for "tool-call"
	Name         string         `json:"name,omitempty"`         // for "tool-call"
	Arguments    string         `json:"arguments,omitempty"`    // for "tool-call" (raw JSON string)
	ToolCallID   string         `json:"toolCallId,omitempty"`   // for "tool-result"
	IsError      bool           `json:"isError,omitempty"`      // for "tool-result"
	Content      []ContentBlock `json:"content,omitempty"`      // nested blocks for "tool-result"
}

// MessageSource identifies who produced a message (upstream MessageSource).
type MessageSource struct {
	Kind        string          `json:"kind"` // "user" | "model" | "tool" | "plugin"
	CallID      string          `json:"callId,omitempty"`
	Plugin      string          `json:"plugin,omitempty"`
	Form        string          `json:"form,omitempty"`
	Provider    string          `json:"provider,omitempty"`
	Model       string          `json:"model,omitempty"`
	ReplayState json.RawMessage `json:"replayState,omitempty"`
}

// WireMessage is the canonical message representation shared by delivery,
// durable history, and model requests (upstream Message). It is also the
// verbatim payload of user/message events (upstream `user/message` data).
type WireMessage struct {
	ID      string         `json:"id"`
	Role    string         `json:"role"` // "user" | "assistant" | "system"
	Content []ContentBlock `json:"content"`
	Source  MessageSource  `json:"source"`
}

// UserMessagePayload defines user input message: the user message verbatim.
type UserMessagePayload = WireMessage

// TurnStartPayload defines the payload for turn/start
type TurnStartPayload struct {
	Turn int `json:"turn"`
}

// TurnEndReason defines possible turn termination reasons
type TurnEndReason struct {
	Kind    string `json:"kind"` // "completed" | "aborted" | "interrupted" | "error"
	Message string `json:"message,omitempty"`
}

// TurnEndPayload defines the payload for turn/end
type TurnEndPayload struct {
	Turn   int           `json:"turn"`
	Reason TurnEndReason `json:"reason"`
}

// StepStartPayload defines the payload for step/start
type StepStartPayload struct {
	Turn int `json:"turn"`
	Step int `json:"step"`
}

// StepEndPayload defines the payload for step/end
type StepEndPayload struct {
	Turn int `json:"turn"`
	Step int `json:"step"`
}

// AssistantMessagePayload defines model response message (upstream `assistant/message` data).
type AssistantMessagePayload struct {
	Turn        int         `json:"turn"`
	Step        int         `json:"step"`
	Message     WireMessage `json:"message"`
	Usage       *TokenUsage `json:"usage,omitempty"`
	Interrupted bool        `json:"interrupted,omitempty"`
}

// ToolCallPayload defines a tool call initiated by the model
type ToolCallPayload struct {
	Turn      int             `json:"turn"`
	Step      int             `json:"step"`
	CallID    string          `json:"callId"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"` // raw JSON string
	// View carries the running-card rendering intent (terminal/diff/text)
	// inferred from the tool name and arguments before execution, so a client
	// can draw a live card while the call is still in flight.
	View *ToolResultView `json:"view,omitempty"`
}

// ToolError carries an internal tool failure identity (upstream `tool/result` error).
type ToolError struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

// ToolResultPayload defines the outcome of a tool execution (upstream `tool/result` data).
type ToolResultPayload struct {
	Turn    int             `json:"turn"`
	Step    int             `json:"step"`
	Message WireMessage     `json:"message"`
	Error   *ToolError      `json:"error,omitempty"`
	View    *ToolResultView `json:"view,omitempty"`
	Meta    json.RawMessage `json:"meta,omitempty"`
}

// ToolResultView carries the rendering intent a client uses to draw a "real
// card" for a tool result (true unified diff, ANSI terminal, or plain text).
type ToolResultView struct {
	Kind      string        `json:"kind"` // "diff" | "terminal" | "text"
	Diffs     []DiffHunk    `json:"diffs,omitempty"`
	Terminal  *TerminalView `json:"terminal,omitempty"`
	Text      string        `json:"text,omitempty"`
}

// DiffHunk is one file's slice of a unified diff (best-effort reconstruction).
type DiffHunk struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// TerminalView renders a command/terminal output card.
type TerminalView struct {
	Lines    []string `json:"lines"`
	ExitCode int      `json:"exitCode"`
}

// TokenUsage records token consumption
type TokenUsage struct {
	InputTokens     int `json:"inputTokens"`
	OutputTokens    int `json:"outputTokens"`
	CacheReadTokens int `json:"cacheReadTokens,omitempty"`
	// CacheWriteTokens counts tokens written into the provider cache during a
	// call (upstream llm/types.ts usage.cacheWriteTokens).
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
	// ReasoningTokens counts tokens spent in a separate reasoning/thinking
	// channel (DeepSeek completion_tokens_details.reasoning_tokens).
	ReasoningTokens int `json:"reasoningTokens,omitempty"`
}

// ModelMessage represents the normalized message structure for LLM requests.
type ModelMessage struct {
	Role    string         `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content []ContentBlock `json:"content"`
}

// DeriveMessages reconstructs LLM conversation history from an append-only
// event stream. It first folds the canonical surface (append/replace) and
// then projects each surviving surface node to its model message — mirroring
// upstream `Session.deriveMessages` over `foldSurface`. Events shadowed by a
// positional replacement never enter model history; replacement copies stay
// model-only.
func DeriveMessages(events []SessionEnvelope) ([]ModelMessage, error) {
	pointers := make([]*SessionEnvelope, len(events))
	for i := range events {
		pointers[i] = &events[i]
	}
	fold, err := FoldSurface(pointers, 0)
	if err != nil {
		return nil, err
	}
	var messages []ModelMessage
	for _, seq := range fold.Nodes {
		env := pointers[seq]
		message, err := deriveEventMessage(env)
		if err != nil {
			return nil, err
		}
		if message != nil {
			messages = append(messages, *message)
		}
	}
	return messages, nil
}

// deriveEventMessage projects one surface event into its LLM message, or
// returns nil when it produces none — the per-node projection rule mirrored
// from upstream `deriveEventMessage`.
func deriveEventMessage(env *SessionEnvelope) (*ModelMessage, error) {
	switch env.Type {
	case EventUserMessage:
		var userMsg UserMessagePayload
		if err := json.Unmarshal(env.Data, &userMsg); err != nil {
			return nil, fmt.Errorf("invalid user/message event data at seq %d: %w", env.Seq, err)
		}
		return &ModelMessage{Role: "user", Content: userMsg.Content}, nil

	case EventAssistantMessage:
		var asstMsg AssistantMessagePayload
		if err := json.Unmarshal(env.Data, &asstMsg); err != nil {
			return nil, fmt.Errorf("invalid assistant/message event data at seq %d: %w", env.Seq, err)
		}
		// An empty-content assistant/message exists only to host a max-tokens
		// step's usage and must not inject a content-less assistant turn into
		if len(asstMsg.Message.Content) == 0 {
			return nil, nil
		}
		return &ModelMessage{Role: "assistant", Content: asstMsg.Message.Content}, nil

	case EventToolResult:
		var toolRes ToolResultPayload
		if err := json.Unmarshal(env.Data, &toolRes); err != nil {
			return nil, fmt.Errorf("invalid tool/result event data at seq %d: %w", env.Seq, err)
		}
		// The tool/result message carries one tool-result block; project it
		// verbatim (nested content included) for the request serializer.
		if len(toolRes.Message.Content) != 1 || toolRes.Message.Content[0].Type != "tool-result" {
			return nil, fmt.Errorf("invalid tool/result event data at seq %d: message.content must be one tool-result block", env.Seq)
		}
		return &ModelMessage{Role: "tool", Content: []ContentBlock{toolRes.Message.Content[0]}}, nil
	}
	return nil, nil
}

func extractTextFromBlocks(blocks []ContentBlock) string {
	var res string
	for _, b := range blocks {
		if b.Type == "text" {
			res += b.Text
		}
	}
	return res
}

// LlmCallConfig is the request-level call configuration snapshot carried by
// `request/header` (upstream LlmCallConfig in dsh-llm/call-config).
type LlmCallConfig struct {
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	ReasoningEffort string   `json:"reasoningEffort,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxTokens       int      `json:"maxTokens,omitempty"`
	Stop            []string `json:"stop,omitempty"`
}

// ToolSchema is one assembled tool declaration in an EpochHeader snapshot.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// EpochHeader is the logged request state outside derived history: call
// config, system prompt, and tool schemas (upstream EpochHeader).
type EpochHeader struct {
	Config LlmCallConfig `json:"config"`
	System string        `json:"system,omitempty"`
	Tools  []ToolSchema  `json:"tools,omitempty"`
}

// RequestHeaderReason marks why a `request/header` snapshot was appended.
type RequestHeaderReason string

const (
	HeaderReasonInitial RequestHeaderReason = "initial"
	HeaderReasonResume  RequestHeaderReason = "resume"
	HeaderReasonChange  RequestHeaderReason = "change"
)

// RequestHeaderPayload is the `request/header` event data.
type RequestHeaderPayload struct {
	Header EpochHeader         `json:"header"`
	Reason RequestHeaderReason `json:"reason"`
}

// ApprovalAskedPayload is the log-only audit event emitted when an approval
// question is put to the answerer chain (upstream approval/asked).
type ApprovalAskedPayload struct {
	ID       string `json:"id"`
	ToolName string `json:"toolName"`
	CallID   string `json:"callId,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// ApprovalDecidedPayload is the log-only audit event appended when the
// outcome of a prior approval/asked is known (upstream approval/decided).
// SandboxMode is the per-session file-policy override (upstream
// dsh-sandbox-policy session-mode: read-only | workspace-write | danger-full-access).
type SandboxMode string

const (
	SandboxReadOnly         SandboxMode = "read-only"
	SandboxWorkspaceWrite   SandboxMode = "workspace-write"
	SandboxDangerFullAccess SandboxMode = "danger-full-access"
)

// SandboxModePayload is the log-only `sandbox/mode` switch (upstream
// session-mode.ts): durable, replayable, never in the model transcript; the
// LAST such event is the session override. source:"delegation" marks an
// override seeded into a child agent at delegation.
type SandboxModePayload struct {
	Mode   SandboxMode `json:"mode"`
	Source string      `json:"source,omitempty"` // "" | "delegation"
}

// ApprovalPolicy is the session approval-policy override (upstream
// user-approval: ask | never). never rejects every ask deterministically.
type ApprovalPolicy string

const (
	ApprovalPolicyAsk   ApprovalPolicy = "ask"
	ApprovalPolicyNever ApprovalPolicy = "never"
)

// ApprovalPolicyPayload is the durable `approval/policy` switch (upstream
// user-approval): log-only, last event wins.
type ApprovalPolicyPayload struct {
	Policy ApprovalPolicy `json:"policy"`
	Source string         `json:"source,omitempty"` // "" | "delegation"
}

// PermissionPresetPayload is the durable `permission/preset` selection
// (upstream permission-presets): log-only user intent; the knob events
// (sandbox/mode, approval/policy) follow in the same turn.
type PermissionPresetPayload struct {
	Preset string `json:"preset"`
}

// CommandSource is who issued a slash command line (upstream CommandSourceMap:
// only the user today).
type CommandSource struct {
	Kind string `json:"kind"` // "user"
}

// CommandRunPayload is the log-only `command/run` lifecycle event: a
// resolved slash command entered its handler (upstream commands types.ts).
type CommandRunPayload struct {
	CommandID string        `json:"commandId"`
	Name      string        `json:"name"`
	Args      string        `json:"args,omitempty"`
	Source    CommandSource `json:"source"`
}

// CommandDonePayload is the paired `command/done` settlement. kind is
// success | error; sourceEventSeq points at the earlier authoritative domain
// event the command produced (upstream commands types.ts).
type CommandDonePayload struct {
	CommandID      string `json:"commandId"`
	Kind           string `json:"kind"`
	Text           string `json:"text,omitempty"`
	SourceEventSeq int    `json:"sourceEventSeq,omitempty"`
}

type ApprovalDecidedPayload struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"` // "allowed-once" | "rejected" | "cancelled" | "unavailable"
}

// TodoItem is one agent todo entry (upstream TodoItem: content + three-state status).
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // "pending" | "in_progress" | "completed"
}

// TodoWritePayload is the whole-list snapshot of `todo/write`.
type TodoWritePayload struct {
	Todos []TodoItem `json:"todos"`
}

// PlanModePayload is the whole-value replace of `plan/mode`.
type PlanModePayload struct {
	Active bool `json:"active"`
}

// GoalChangePayload is the durable `goal/change` snapshot (upstream
// GoalSnapshotChangeMeta / GoalClearChangeMeta union).
type GoalChangePayload struct {
	Kind          string        `json:"kind"` // "goal/change"
	Version       int           `json:"version"`
	Operation     string        `json:"operation"` // "create"|"edit"|"pause"|"resume"|"complete"|"block"|"clear"
	Goal          *GoalSnapshot `json:"goal,omitempty"`
	RoundsStarted int           `json:"roundsStarted,omitempty"`
	CreatedAt     int64         `json:"createdAt,omitempty"`
	UpdatedAt     int64         `json:"updatedAt,omitempty"`
	Cleared       *GoalRef      `json:"cleared,omitempty"`
	ClearedAt     int64         `json:"clearedAt,omitempty"`
}

// TeamMemberPayload is the whole durable teammate lifecycle value
// (upstream agent-team team/member: stored in the Team Lead session).
type TeamMemberPayload struct {
	Version int              `json:"version"`
	TeamID  string           `json:"teamId"`
	Member  TeamMemberRecord `json:"member"`
}

// TeamMemberRecord is one immutable durable teammate snapshot.
type TeamMemberRecord struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Provider    string `json:"provider"`
	Context     string `json:"context"` // "fresh" | "fork"
	Phase       string `json:"phase"`   // "provisioning" | "active" | "failed"
	Error       string `json:"error,omitempty"`
}

// TeamTaskPayload is the whole shared-task value (upstream team/task).
type TeamTaskPayload struct {
	Version int            `json:"version"`
	TeamID  string         `json:"teamId"`
	Task    TeamTaskRecord `json:"task"`
}

// TeamTaskRecord is one durable task snapshot; every mutation bumps Revision.
type TeamTaskRecord struct {
	ID          string   `json:"id"`
	Revision    int      `json:"revision"`
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	Status      string   `json:"status"` // "pending" | "in_progress" | "completed" | "deleted"
	OwnerID     string   `json:"ownerId,omitempty"`
	BlockedBy   []string `json:"blockedBy"`
	WriteScopes []string `json:"writeScopes"`
}

// TeamMessagePayload is the durable mailbox enqueue (upstream
// team/message/queued).
type TeamMessagePayload struct {
	Version int               `json:"version"`
	TeamID  string            `json:"teamId"`
	Message TeamMessageRecord `json:"message"`
}

// TeamMessageRecord is one durable peer message retained until the target
// session records it.
type TeamMessageRecord struct {
	ID         string         `json:"id"`
	SenderID   string         `json:"senderId"`
	SenderName string         `json:"senderName"`
	TargetID   string         `json:"targetId"`
	Delivery   string         `json:"delivery"` // "quiet" | "wakeup"
	Content    []ContentBlock `json:"content"`
}

// TeamMessageDeliveredPayload is the durable delivery acknowledgement
// (upstream team/message/delivered).
type TeamMessageDeliveredPayload struct {
	Version   int    `json:"version"`
	TeamID    string `json:"teamId"`
	MessageID string `json:"messageId"`
	TargetID  string `json:"targetId"`
}

// FeedbackRecordPayload is one recorded human remark about this session
// (upstream command-feedback: log-only, never enters model context).
type FeedbackRecordPayload struct {
	Text string `json:"text"`
}

// ScheduleRecord is one v1 schedule/change record (upstream ScheduleRecord
// union: after | at | every). ScheduledAt is a canonical four-digit-year UTC
// instant.
type ScheduleRecord struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"` // "after" | "at" | "every"
	Prompt       string `json:"prompt"`
	ScheduledAt  string `json:"scheduledAt"`
	AfterSeconds *int64 `json:"afterSeconds,omitempty"`
	EverySeconds *int64 `json:"everySeconds,omitempty"`
}

// ScheduleChangePayload is the durable schedule/change snapshot (upstream
// ScheduleChange union: create | delete | dispatch).
type ScheduleChangePayload struct {
	Version    int             `json:"version"`
	Operation  string          `json:"operation"` // "create" | "delete" | "dispatch"
	Schedule   *ScheduleRecord `json:"schedule,omitempty"`
	ID         string          `json:"id,omitempty"`
	AcceptedAt string          `json:"acceptedAt,omitempty"`
}

// GoalRef identifies one goal across durable revisions.
type GoalRef struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

// GoalSnapshot is the full durable state written by every non-clear goal mutation.
type GoalSnapshot struct {
	ID            string           `json:"id"`
	Revision      int              `json:"revision"`
	Objective     string           `json:"objective"`
	Phase         string           `json:"phase"` // "active"|"paused"|"blocked"|"complete"
	BlockedReason *GoalBlockReason `json:"blockedReason,omitempty"`
	MaxGoalRounds int              `json:"maxGoalRounds"`
}

// GoalBlockReason is the machine-routable explanation for a blocked goal.
type GoalBlockReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
