package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"dsh-go/pkg/session"
)

// ApprovalDecision represents the human or policy approval outcome.
type ApprovalDecision string

const (
	ApprovalAllowOnce ApprovalDecision = "allow_once"
	ApprovalAllowAll  ApprovalDecision = "allow_all"
	ApprovalDeny      ApprovalDecision = "deny"
	ApprovalCancel    ApprovalDecision = "cancel"
)

// UserQuestionOption is one selectable choice offered with a structured
// question. ID mirrors the label today (the wire schema has no separate
// optionId); the field exists so hosts can grow stable ids without another
// signature break.
type UserQuestionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// UserQuestion is one question handed to a UserQuestionAnswerer (the
// structured upgrade path of the legacy approval waterfall).
type UserQuestion struct {
	ID          string               `json:"id"`
	Header      string               `json:"header,omitempty"`
	Prompt      string               `json:"prompt"`
	Options     []UserQuestionOption `json:"options,omitempty"`
	MultiSelect bool                 `json:"multiSelect"`
}

// UserQuestionAnswer is one structured answer: selected option id(s) or a
// free-text custom answer. The zero value (empty ID) means the user
// dismissed/cancelled the dialog — every real answer echoes the question id.
type UserQuestionAnswer struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected,omitempty"`
	Custom   string   `json:"custom,omitempty"`
}

// Cancelled reports whether the answer carries no user choice (dismissed
// dialog, host collapse, or empty provider reply).
func (a UserQuestionAnswer) Cancelled() bool {
	return a.ID == "" && len(a.Selected) == 0 && a.Custom == ""
}

// UserQuestionAnswerer is the structured question/answer bridge for hosts
// (GUI dialogs, ACP clients) that can carry custom options and typed answers
// instead of collapsing them into the allow/deny/cancel ApprovalDecision
// enum. Hosts opt in by storing any value implementing this interface in
// ToolExecutionContext.Answerer — the interface and types are defined here in
// pkg/tools so sibling packages (pkg/gateway, pkg/acp) need NO signature
// changes; they adopt it incrementally via a plain type assertion.
type UserQuestionAnswerer interface {
	RequestUserStructured(question UserQuestion) ([]UserQuestionAnswer, error)
}

// defaultToolTimeout is the fallback execution budget for tools that declare
// neither TimeoutMs nor NoTimeout.
const defaultToolTimeout = 60 * time.Second

// noTimeoutByDesign names tools whose design budget intentionally exceeds any
// fixed deadline (they orchestrate whole agent loops / subagent fan-outs that
// own their internal budgets). Their registration literals live outside this
// package's editable surface (workflow_run -> workflow_tool.go,
// invoke_subagent -> pkg/subagent/manager.go), so the exemption is applied by
// name here instead of a TimeoutMs declaration on the definition.
var noTimeoutByDesign = map[string]bool{
	"workflow_run":    true,
	"invoke_subagent": true,
}

// ToolExecutionContext carries contextual data during tool execution.
type ToolExecutionContext struct {
	Context     context.Context
	SessionID   string
	Cwd         string
	Turn        int
	Step        int
	CallID      string
	RequestUser func(prompt string, options []string) (ApprovalDecision, error)
	// Emit appends one log-only session event (approval audit, todo/plan/goal
	// domain snapshots). The agent loop injects it; nil disables emission
	// (direct tool tests).
	Emit func(eventType string, payload any)
	// Events returns the caller session's ordered event log (ring or
	// persisted replay) for fold-based tools; nil falls back to the
	// registered session provider.
	Events func() []*session.SessionEnvelope
	// CallerID / CallerName identify the executing agent inside one Team;
	// ParentSessionID is the team lead for a teammate caller.
	CallerID        string
	CallerName      string
	ParentSessionID string
	// Answerer optionally carries a structured question/answer bridge for
	// interactive tools (ask_user custom options). Any value implementing
	// UserQuestionAnswerer is honored via type assertion; nil (or any other
	// type) keeps the legacy RequestUser approval semantics untouched.
	Answerer any
}

// ToolDefinition defines the contract for a model-invocable tool.
type ToolDefinition struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	ParametersJSON json.RawMessage `json:"parameters"`
	Execute        func(ctx ToolExecutionContext, argsJSON string) (any, error)
	RequiresPerm   bool   `json:"requiresPerm"`
	Owner          string `json:"owner,omitempty"`
	// TimeoutMs declares this tool's own execution budget in milliseconds;
	// ExecutePipeline arms exactly this deadline instead of the shared
	// defaultToolTimeout. Declare slightly MORE than the tool's internal
	// budget (e.g. terminal_send settles at 120s -> declare 150000) so the
	// inner timeout produces the meaningful error first.
	TimeoutMs int64
	// NoTimeout opts this tool out of any pipeline-imposed deadline. Only
	// for orchestrators that run whole agent loops and manage their own
	// budgets; ordinary tools should declare TimeoutMs instead.
	NoTimeout bool
}

// ToolRegistry manages registered tools and runs the execution pipeline.
type ToolRegistry struct {
	tools  map[string]ToolDefinition
	owners map[string]string
	defs   map[string]ToolDefinition
	mu     sync.RWMutex
	Policy *PolicyStore
	// Commands is the shared slash-command registry; every frontend
	// (TUI, gateway RPC, Godot) resolves /lines through it so the
	// command/run -> command/done lifecycle lands in the session log once.
	Commands *CommandRegistry
	// ReviewTool is the small-model review seam for the "auto" (review)
	// approval policy. It is injected by the host (gateway/TUI/headless) with
	// a call into the shared LLM adapter; when nil, review mode falls back to
	// asking the user for destructive tools (fail-closed, never auto-allow).
	ReviewTool func(sessionID, reviewModel, toolName, argsJSON string, timeout time.Duration) ReviewVerdict
}

// ReviewVerdict is the small-model safety verdict for a destructive tool.
type ReviewVerdict int

const (
	ReviewUncertain ReviewVerdict = iota // escalate to the user
	ReviewAllow                          // safe enough to auto-run
	ReviewDeny                           // refuse
)

func (r ReviewVerdict) String() string {
	switch r {
	case ReviewAllow:
		return "allow"
	case ReviewDeny:
		return "deny"
	default:
		return "uncertain"
	}
}

// isEditTool reports whether a tool is a file-edit/write tool — the family the
// accept-edits policy auto-approves without asking.
func isEditTool(name string) bool {
	switch name {
	case "write_file", "replace_file_content":
		return true
	}
	return false
}

// isDestructiveTool reports whether a tool can mutate state outside the
// workspace (shell, persistent shells, deletions, destructive git subcommands,
// MCP writes) — the family the auto/review policy routes to the small model.
func isDestructiveTool(name string) bool {
	switch name {
	case "run_command", "bash", "pwsh", "bash_persistent", "pwsh_persistent",
		"delete_file", "delete_dir", "remove", "rm",
		"run_shell", "terminal_send", "shell", "git_commit", "git_discard",
		"git_stage", "git_unstage", "git_restore", "jobs_kill", "schedule_create",
		"schedule_enable", "schedule_disable", "schedule_delete":
		return true
	}
	return false
}

// reviewToolCall resolves the small-model verdict for one destructive tool
// call. A nil ReviewTool or empty reviewModel always escalates to uncertain
// (the user is asked), never silently auto-allows.
func (r *ToolRegistry) reviewToolCall(ctx ToolExecutionContext, name, argsJSON string, pol SessionPolicy) ReviewVerdict {
	if r.ReviewTool == nil {
		return ReviewUncertain
	}
	if pol.ReviewModel == "" {
		return ReviewUncertain
	}
	return r.ReviewTool(ctx.SessionID, pol.ReviewModel, name, argsJSON, reviewTimeout)
}

const reviewTimeout = 3 * time.Second

// NewToolRegistry initializes a tool registry with standard builtin tools.
func NewToolRegistry() *ToolRegistry {
	r := NewToolRegistryEmpty()
	r.RegisterBuiltinTools()
	return r
}

// NewToolRegistryEmpty creates the shared registries without mounting builtin
// families. Production hosts use this with the plugin registry so each family
// has an owner and disposer.
func NewToolRegistryEmpty() *ToolRegistry {
	return &ToolRegistry{
		tools:    make(map[string]ToolDefinition),
		owners:   make(map[string]string),
		Policy:   NewPolicyStore(),
		Commands: NewCommandRegistry(),
	}
}

// RegisterBuiltinTools mounts the legacy implementations as one migration
// unit. The plugin host owns this unit in production; the legacy constructor
// calls it for compatibility with package consumers.
func (r *ToolRegistry) RegisterBuiltinTools() {
	r.Commands.RegisterBuiltinCommands()
	r.registerBuiltins()
	r.RegisterFSTools()
	r.RegisterTaskTools()
	r.RegisterPersistentShellTools()
	r.RegisterJobTools()
	r.RegisterPolicyTools()
	r.RegisterTerminalTools()
	r.RegisterAskUserTool()
	r.RegisterGoalTools()
	r.RegisterGlobTool()
	r.RegisterExitPlanModeTool()
	r.RegisterScheduleTools()
	// Phase 2（第二轮迁移 B/C 缺口）补齐的工具族：
	// TODO(Phase2): migrate these tool families into Core-backed capabilities so
	// the tool registry is assembled exclusively through plugin mounts.
	r.RegisterImageTools()        // 视觉读图 read_image
	r.RegisterPwshTools()         // pwsh 持久化会话
	r.RegisterSessionQueryTools() // session_search / session_trace / session_event_read
	r.RegisterSkillTools()        // skill / skill_list
	r.RegisterTeamTools()         // Agent Teams 运行时工具（spawn_teammate 等）
	r.RegisterWorkflowTools()     // workflow_run（subagent 工作流编排）
	return
}

// Register registers a tool definition.
func (r *ToolRegistry) Register(tool ToolDefinition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners == nil {
		r.owners = map[string]string{}
	}
	delete(r.owners, tool.Name)
	tool.Owner = ""
	r.tools[tool.Name] = tool
}

// Names returns a stable snapshot of registered tool names.
func (r *ToolRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Get looks up a tool by name.
func (r *ToolRegistry) Get(name string) (ToolDefinition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return ToolDefinition{}, false
	}
	t.Owner = r.owners[name]
	return t, true
}

// ToolOwners returns a snapshot of owner assignments.
func (r *ToolRegistry) ToolOwners() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.owners))
	for k, v := range r.owners {
		out[k] = v
	}
	return out
}

// ListDeclarations returns all registered tool schemas for LLM prompts.
func (r *ToolRegistry) ListDeclarations() []ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		t.Owner = r.owners[t.Name]
		list = append(list, t)
	}
	return list
}

// ExecutePipeline executes a tool through the guarded multi-stage pipeline.
func (r *ToolRegistry) ExecutePipeline(ctx ToolExecutionContext, name string, argsJSON string) (string, bool, error) {
	tool, ok := r.Get(name)
	if !ok {
		return fmt.Sprintf("Error: unknown tool '%s'", name), true, nil
	}

	// 1. Permission / Approval Stage. The session's resolved approval policy
	// decides whether this RequiresPerm tool runs without asking:
	//   - never:         deterministic reject (read-only / plan)
	//   - allow-all:     auto-allow everything (YOLO / full access)
	//   - accept-edits:  auto-allow edit/write tools; commands still ask
	//   - review:        auto-allow safe tools; destructive tools go to the
	//                    small review model (allow / deny / uncertain -> ask)
	//   - ask:           delegate to the answerer chain (default)
	//   - (per-tool allow_all whitelist): skip the ask for a previously
	//     "always allow" tool regardless of the policy above.
	pol := SessionPolicy{Sandbox: DefaultSandboxMode, Approval: DefaultApprovalPolicy}
	if r.Policy != nil {
		pol = r.Policy.Get(ctx.SessionID)
	}
	if tool.RequiresPerm {
		approvalPolicy := pol.Approval
		remembered := r.Policy != nil && r.Policy.PolicyAllowed(ctx.SessionID, name)
		var ask bool
		switch approvalPolicy {
		case session.ApprovalPolicyNever:
			approvalID := fmt.Sprintf("approval-%d", time.Now().UnixNano())
			if ctx.Emit != nil {
				ctx.Emit("approval/asked", session.ApprovalAskedPayload{
					ID: approvalID, ToolName: name, CallID: ctx.CallID, Reason: "approval-policy: never",
				})
				ctx.Emit("approval/decided", session.ApprovalDecidedPayload{ID: approvalID, Outcome: "rejected"})
			}
			return "Permission denied by approval policy (never).", true, nil
		case session.ApprovalPolicyAllowAll:
			ask = false // full access: no prompt
		case session.ApprovalPolicyAcceptEdits:
			if isEditTool(name) {
				ask = false // edit/write tools auto-allowed
			} else {
				ask = !remembered
			}
		case session.ApprovalPolicyReview:
			switch {
			case remembered:
				ask = false
			case !isDestructiveTool(name):
				ask = false // safe tools auto-allowed
			default:
				// Destructive tool: ask the small review model first.
				review := r.reviewToolCall(ctx, name, argsJSON, pol)
				switch review {
				case ReviewAllow:
					ask = false
				case ReviewDeny:
					approvalID := fmt.Sprintf("approval-%d", time.Now().UnixNano())
					if ctx.Emit != nil {
						ctx.Emit("approval/asked", session.ApprovalAskedPayload{
							ID: approvalID, ToolName: name, CallID: ctx.CallID, Reason: "approval-policy: review(deny)",
						})
						ctx.Emit("approval/decided", session.ApprovalDecidedPayload{ID: approvalID, Outcome: "rejected"})
					}
					return "Permission denied by auto-review model.", true, nil
				default: // uncertain -> escalate to the user
					ask = true
				}
			}
		default: // ask
			ask = !remembered
		}
		if !ask {
			goto execution
		}
		// 无 answerer 时不拦：保持既有语义（headless/无审批宿主仍可跑工具）。
		if ctx.RequestUser == nil {
			goto execution
		}
		// Log-only audit pair (upstream approval/asked -> approval/decided).
		approvalID := fmt.Sprintf("approval-%d", time.Now().UnixNano())
		if ctx.Emit != nil {
			ctx.Emit("approval/asked", session.ApprovalAskedPayload{
				ID:       approvalID,
				ToolName: name,
				CallID:   ctx.CallID,
				Reason:   "user-approval waterfall",
			})
		}
		promptArgs := argsJSON
		if len(promptArgs) > 4000 {
			promptArgs = promptArgs[:1800] + "…" + promptArgs[len(promptArgs)-1800:]
		}
		decision, err := ctx.RequestUser(fmt.Sprintf("Allow tool '%s' with args: %s?", name, promptArgs), []string{"allow_once", "allow_all", "deny", "cancel"})
		outcome := "cancelled"
		switch decision {
		case ApprovalAllowOnce:
			outcome = "allowed-once"
		case ApprovalAllowAll:
			outcome = "allowed-always"
			if r.Policy != nil {
				r.Policy.RememberApproval(ctx.SessionID, name)
			}
		case ApprovalDeny:
			outcome = "rejected"
		case ApprovalCancel:
			outcome = "cancelled"
		}
		if ctx.Emit != nil {
			ctx.Emit("approval/decided", session.ApprovalDecidedPayload{ID: approvalID, Outcome: outcome})
		}
		if err != nil || decision == ApprovalDeny || decision == ApprovalCancel {
			return "Permission denied by user.", true, nil
		}
	}
execution:

	// Per-tool budget: a declared TimeoutMs (or an explicit NoTimeout opt-out)
	// wins over the shared default, so tools whose internal design budgets
	// exceed 60s (terminal_send 120s, job_output waits up to 600s) are never
	// truncated by the pipeline before their own deadline fires.
	execCtx := ctx.Context
	var cancel context.CancelFunc
	switch {
	case tool.NoTimeout || noTimeoutByDesign[name]:
		// Orchestrators that own their internal budgets run unbounded.
		cancel = func() {}
	case tool.TimeoutMs > 0:
		execCtx, cancel = context.WithTimeout(ctx.Context, time.Duration(tool.TimeoutMs)*time.Millisecond)
	default:
		execCtx, cancel = context.WithTimeout(ctx.Context, defaultToolTimeout)
	}
	defer cancel()
	ctx.Context = execCtx

	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		return fmt.Sprintf("Tool execution error: %v", err), true, nil
	}

	// 3. Finalize Content (Convert to canonical string)
	var outputStr string
	switch v := result.(type) {
	case string:
		outputStr = v
	default:
		b, _ := json.MarshalIndent(v, "", "  ")
		outputStr = string(b)
	}

	return outputStr, false, nil
}

func (r *ToolRegistry) registerBuiltins() {
	// 1. read_file
	r.Register(ToolDefinition{
		Name:        "read_file",
		Description: "Read file contents at specified path.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": { "type": "string", "description": "Absolute or relative file path" }
			},
			"required": ["path"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			targetPath := args.Path
			if !filepath.IsAbs(targetPath) && ctx.Cwd != "" {
				targetPath = filepath.Join(ctx.Cwd, targetPath)
			}
			data, err := os.ReadFile(targetPath)
			if err != nil {
				return nil, err
			}
			return string(data), nil
		},
	})

	// 2. write_file
	r.Register(ToolDefinition{
		Name:         "write_file",
		Description:  "Write contents to a file (creates parent directories if missing).",
		RequiresPerm: true,
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": { "type": "string", "description": "Target file path" },
				"content": { "type": "string", "description": "File content" }
			},
			"required": ["path", "content"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			targetPath := args.Path
			if !filepath.IsAbs(targetPath) && ctx.Cwd != "" {
				targetPath = filepath.Join(ctx.Cwd, targetPath)
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(targetPath, []byte(args.Content), 0644); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Successfully wrote %d bytes to %s", len(args.Content), args.Path), nil
		},
	})

	// 3. run_command is a separate family so production can mount it
	// from the terminal capability without re-registering the FS stubs.
	r.RegisterRunCommandTool()
}

// RegisterRunCommandTool mounts the one-shot shell tool (fresh process per call).
func (r *ToolRegistry) RegisterRunCommandTool() {
	r.Register(ToolDefinition{
		Name:         "run_command",
		Description:  "Execute a shell command and return its stdout/stderr. Each call runs in a fresh shell: no state (cwd, variables, functions) persists between calls — pass workdir instead of using cd. Non-zero exits are reported as [exit code: N]. Set run_in_background:true for long-running commands: the call returns a job id immediately; read its output with job_output and stop it with job_kill.",
		RequiresPerm: true,
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": { "type": "string", "description": "Command string to execute" },
				"description": { "type": "string", "description": "One-sentence description of what this command does" },
				"timeout_ms": { "type": "integer", "description": "Optional timeout in milliseconds" },
				"workdir": { "type": "string", "description": "Working directory for this call" },
				"run_in_background": { "type": "boolean", "description": "Return a job id immediately; collect with job_output or stop with job_kill" }
			},
			"required": ["command", "description"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Command         string `json:"command"`
				Description     string `json:"description"`
				TimeoutMs       int    `json:"timeout_ms"`
				Workdir         string `json:"workdir"`
				RunInBackground bool   `json:"run_in_background"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if args.Command == "" {
				return nil, fmt.Errorf("command must be a non-empty string")
			}
			if args.Description == "" {
				return nil, fmt.Errorf("description must be a non-empty string")
			}

			shellName := "powershell"
			shellArgs := []string{"-NoProfile", "-Command", args.Command}
			if runtime.GOOS != "windows" {
				shellName = "bash"
				shellArgs = []string{"-c", args.Command}
			}
			workdir := ctx.Cwd
			if args.Workdir != "" {
				if filepath.IsAbs(args.Workdir) {
					workdir = args.Workdir
				} else if workdir != "" {
					workdir = filepath.Join(workdir, args.Workdir)
				}
			}
			execCtx := ctx.Context
			if args.TimeoutMs > 0 {
				var cancel func()
				execCtx, cancel = context.WithTimeout(ctx.Context, time.Duration(args.TimeoutMs)*time.Millisecond)
				defer cancel()
			}

			if args.RunInBackground {
				cmd := exec.Command(shellName, shellArgs...)
				makeProcessGroup(cmd)
				if workdir != "" {
					cmd.Dir = workdir
				}
				job, err := startJob(ctx.SessionID, "bash", args.Description, cmd)
				if err != nil {
					return nil, err
				}
				return fmt.Sprintf("started background job %s", job.ID), nil
			}

			cmd := exec.CommandContext(execCtx, shellName, shellArgs...)
			if workdir != "" {
				cmd.Dir = workdir
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				if execCtx.Err() != nil {
					return fmt.Sprintf("Command timed out:\n%s", string(output)), nil
				}
				exitCode := 1
				if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
					exitCode = cmd.ProcessState.ExitCode()
				}
				return fmt.Sprintf("[exit code: %d]\n%s", exitCode, string(output)), nil
			}
			return string(output), nil
		},
	})
}

// RegisterOwned registers a tool with an owner and returns a conditional disposer.
// RegisterOwned registers a tool with an owner and returns a conditional disposer.
func (r *ToolRegistry) RegisterOwned(owner string, tool ToolDefinition) func() {
	if owner == "" || tool.Name == "" {
		return func() {}
	}
	r.mu.Lock()
	if r.owners == nil {
		r.owners = map[string]string{}
	}
	tool.Owner = owner
	r.tools[tool.Name] = tool
	r.owners[tool.Name] = owner
	r.mu.Unlock()
	return func() { r.UnregisterOwned(owner, tool.Name) }
}

// ClaimOwner associates existing tool definitions with an owner after a
// compatibility family has mounted them.
func (r *ToolRegistry) ClaimOwner(owner string, names ...string) {
	if owner == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners == nil {
		r.owners = map[string]string{}
	}
	for _, name := range names {
		if _, ok := r.tools[name]; ok {
			r.owners[name] = owner
		}
	}
}

func (r *ToolRegistry) UnregisterOwned(owner, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners[name] != owner {
		return
	}
	delete(r.tools, name)
	delete(r.owners, name)
}

func (r *ToolRegistry) RegisterChecked(tool ToolDefinition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners == nil {
		r.owners = map[string]string{}
	}
	if _, exists := r.tools[tool.Name]; exists {
		return fmt.Errorf("tool %q 已存在", tool.Name)
	}
	r.tools[tool.Name] = tool
	return nil
}

// Unregister 注销工具；不存在时静默。
func (r *ToolRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.owners, name)
	delete(r.tools, name)
}
