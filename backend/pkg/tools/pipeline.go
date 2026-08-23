package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
}

// ToolDefinition defines the contract for a model-invocable tool.
type ToolDefinition struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	ParametersJSON json.RawMessage `json:"parameters"`
	Execute        func(ctx ToolExecutionContext, argsJSON string) (any, error)
	RequiresPerm   bool `json:"requiresPerm"`
}

// ToolRegistry manages registered tools and runs the execution pipeline.
type ToolRegistry struct {
	tools  map[string]ToolDefinition
	mu     sync.RWMutex
	Policy *PolicyStore
	// Commands is the shared slash-command registry; every frontend
	// (TUI, gateway RPC, Godot) resolves /lines through it so the
	// command/run -> command/done lifecycle lands in the session log once.
	Commands *CommandRegistry
}

// NewToolRegistry initializes a tool registry with standard builtin tools.
func NewToolRegistry() *ToolRegistry {
	r := &ToolRegistry{
		tools:    make(map[string]ToolDefinition),
		Policy:   NewPolicyStore(),
		Commands: NewCommandRegistry(),
	}
	r.Commands.RegisterBuiltinCommands()
	r.registerBuiltins()
	r.RegisterFSTools()
	r.RegisterWebTools()
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
	return r
}

// Register registers a tool definition.
func (r *ToolRegistry) Register(tool ToolDefinition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name] = tool
}

// Get looks up a tool by name.
func (r *ToolRegistry) Get(name string) (ToolDefinition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// ListDeclarations returns all registered tool schemas for LLM prompts.
func (r *ToolRegistry) ListDeclarations() []ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var list []ToolDefinition
	for _, t := range r.tools {
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

	// 1. Permission / Approval Stage (session approval-policy knob:
	// upstream effectiveApprovalPolicy 鈥?"never" rejects deterministically
	// without asking, "ask" delegates to the answerer chain).
	if tool.RequiresPerm {
		approvalPolicy := DefaultApprovalPolicy
		if r.Policy != nil {
			approvalPolicy = r.Policy.Get(ctx.SessionID).Approval
		}
		if approvalPolicy == session.ApprovalPolicyNever {
			approvalID := fmt.Sprintf("approval-%d", time.Now().UnixNano())
			if ctx.Emit != nil {
				ctx.Emit("approval/asked", session.ApprovalAskedPayload{
					ID: approvalID, ToolName: name, CallID: ctx.CallID, Reason: "approval-policy: never",
				})
				ctx.Emit("approval/decided", session.ApprovalDecidedPayload{ID: approvalID, Outcome: "rejected"})
			}
			return "Permission denied by approval policy (never).", true, nil
		}
	}
	if tool.RequiresPerm && ctx.RequestUser != nil {
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
		decision, err := ctx.RequestUser(fmt.Sprintf("Allow tool '%s' with args: %s?", name, argsJSON), []string{"allow_once", "deny", "cancel"})
		outcome := "cancelled"
		switch decision {
		case ApprovalAllowOnce:
			outcome = "allowed-once"
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
	} // 2. Execution Stage with timeout
	execCtx, cancel := context.WithTimeout(ctx.Context, 60*time.Second)
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

	// 3. run_command (upstream @deepseek-ai/dsh-tool-bash contract)
	r.Register(ToolDefinition{
		Name:         "run_command",
		Description:  "Execute a shell command and return its stdout/stderr. Each call runs in a fresh shell: no state (cwd, variables, functions) persists between calls 鈥?pass workdir instead of using cd. Non-zero exits are reported as [exit code: N]. Set run_in_background:true for long-running commands: the call returns a job id immediately; read its output with job_output and stop it with job_kill.",
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

// RegisterChecked 注册工具；名称冲突时返回错误而非静默覆盖（MCP 两阶段同步使用）。
func (r *ToolRegistry) RegisterChecked(tool ToolDefinition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	delete(r.tools, name)
}
