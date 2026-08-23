package tools

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"dsh-go/pkg/session"
)

// CommandResult is one settled slash-command outcome (upstream CommandResult
// union: success carries optional text and the earlier authoritative domain
// event; error carries rendered failure text).
type CommandResult struct {
	Kind      string // "success" | "error"
	Text      string
	SourceSeq int // seq of the authoritative domain event (plan/mode etc.)
}

// CommandInvocation is the handler input for one slash-command execution.
type CommandInvocation struct {
	SessionID string
	Cwd       string
	RawInput  string // exact text after the command name, including separator
	// Emit appends one log-only session event; the agent loop injects it.
	Emit func(eventType string, payload any)
	// EmitSeq is like Emit but returns the assigned event seq (for
	// sourceEventSeq); nil falls back to Emit with seq 0.
	EmitSeq func(eventType string, payload any) (int, error)
	// Policy is the shared policy store; the permission command writes knobs
	// through it (nil disables).
	Policy *PolicyStore
}

// CommandDefinition is one registered slash command (upstream
// CommandDefinition: lowercase name without the leading slash).
type CommandDefinition struct {
	Name        string
	Description string
	Handler     func(inv CommandInvocation) CommandResult
}

// CommandRegistry owns the slash-command table and the lifecycle event pair
// command/run -> command/done (upstream @deepseek-ai/dsh-commands).
type CommandRegistry struct {
	mu   sync.RWMutex
	cmds map[string]CommandDefinition
}

// NewCommandRegistry builds an empty registry.
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{cmds: map[string]CommandDefinition{}}
}

// Register adds a command definition; a duplicate name is replaced (last
// wins, matching scoped-layer shadowing semantics).
func (cr *CommandRegistry) Register(def CommandDefinition) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.cmds[def.Name] = def
}

// List returns name-sorted descriptors for discovery UI.
func (cr *CommandRegistry) List() []CommandDefinition {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	out := make([]CommandDefinition, 0, len(cr.cmds))
	for _, def := range cr.cmds {
		out = append(out, def)
	}
	return out
}

// ParseCommand splits one slash-command line: name without the slash and the
// exact raw input after it. Malformed lines (no leading slash, empty name)
// return ok=false; admission misses log nothing (upstream parseCommand).
func ParseCommand(line string) (name, rawInput string, ok bool) {
	if !strings.HasPrefix(line, "/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(line, "/")
	sp := strings.IndexAny(rest, " \t")
	if sp < 0 {
		name = rest
	} else {
		name = rest[:sp]
		rawInput = rest[sp+1:]
	}
	if name == "" {
		return "", "", false
	}
	return name, rawInput, true
}

// Execute runs one slash-command line against the registry. The lifecycle
// pair command/run -> command/done is appended only when the name resolves
// (upstream: admission misses log nothing). Returns the settled result or
// nil when the line is not a command or the name is unknown.
func (cr *CommandRegistry) Execute(inv CommandInvocation, line string) *CommandResult {
	name, rawInput, ok := ParseCommand(line)
	if !ok {
		return nil
	}
	// The handler receives the parsed raw input (upstream
	// CommandInvocation.rawInput: exact text after the name, including
	// separator whitespace).
	inv.RawInput = rawInput
	cr.mu.RLock()
	def, exists := cr.cmds[name]
	cr.mu.RUnlock()
	if !exists {
		return nil
	}
	commandID := fmt.Sprintf("command-%d-%s", time.Now().UnixNano(), name)
	// command/run is appended before the handler runs (upstream lifecycle).
	runPayload := session.CommandRunPayload{
		CommandID: commandID,
		Name:      name,
		Args:      rawInput,
		Source:    session.CommandSource{Kind: "user"},
	}
	if inv.EmitSeq != nil {
		_, _ = inv.EmitSeq(session.EventCommandRun, runPayload)
	} else if inv.Emit != nil {
		inv.Emit(session.EventCommandRun, runPayload)
	}

	var res CommandResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				res = CommandResult{Kind: "error", Text: fmt.Sprintf("/%s panicked: %v", name, r)}
			}
		}()
		res = def.Handler(inv)
	}()
	if res.Kind == "" {
		res.Kind = "success"
	}

	// command/done settles the pair (a thrown handler settles as error).
	done := session.CommandDonePayload{CommandID: commandID, Kind: res.Kind}
	if res.Text != "" {
		done.Text = res.Text
	}
	if res.SourceSeq > 0 {
		done.SourceEventSeq = res.SourceSeq
	}
	if inv.EmitSeq != nil {
		_, _ = inv.EmitSeq(session.EventCommandDone, done)
	} else if inv.Emit != nil {
		inv.Emit(session.EventCommandDone, done)
	}
	return &res
}

// RegisterBuiltinCommands installs the upstream command set: plan, permission,
// exit and help. plan/permission are domain commands writing the canonical
// events; exit/help are frontend conveniences resolved here so every UI
// shares one registry.
func (cr *CommandRegistry) RegisterBuiltinCommands() {
	cr.Register(CommandDefinition{
		Name:        "plan",
		Description: "Enter or leave plan mode; /plan <message> records the plan.",
		Handler: func(inv CommandInvocation) CommandResult {
			message := strings.TrimSpace(inv.RawInput)
			active := message != "off"
			emit := func(ev string, p any) (int, error) {
				if inv.EmitSeq != nil {
					return inv.EmitSeq(ev, p)
				}
				if inv.Emit != nil {
					inv.Emit(ev, p)
				}
				return 0, nil
			}
			seq, err := emit(session.EventPlanMode, session.PlanModePayload{Active: active})
			if err != nil {
				return CommandResult{Kind: "error", Text: fmt.Sprintf("plan mode switch failed: %v", err)}
			}
			setSessionPlanMode(inv.SessionID, active)
			if active && message != "" {
				// The plan text is the authoritative payload; keep the live
				// mirror so a later model turn reads it back.
				setSessionPlan(inv.SessionID, message)
			}
			if active {
				return CommandResult{Kind: "success", Text: "Plan mode on.", SourceSeq: seq}
			}
			return CommandResult{Kind: "success", Text: "Plan mode off.", SourceSeq: seq}
		},
	})

	cr.Register(CommandDefinition{
		Name:        "permission",
		Description: "Apply a permission preset: default (workspace-write + ask) | strict (read-only + never) | unrestricted (danger-full-access + ask).",
		Handler: func(inv CommandInvocation) CommandResult {
			preset := strings.TrimSpace(inv.RawInput)
			if inv.Policy == nil {
				return CommandResult{Kind: "error", Text: "/permission: policy store unavailable"}
			}
			switch preset {
			case "default":
				_ = inv.Policy.SetPreset(inv.SessionID, preset, inv.Emit)
				if err := inv.Policy.SetSandboxMode(inv.SessionID, session.SandboxWorkspaceWrite, "", inv.Emit); err != nil {
					return CommandResult{Kind: "error", Text: err.Error()}
				}
				_ = inv.Policy.SetApprovalPolicy(inv.SessionID, session.ApprovalPolicyAsk, "", inv.Emit)
			case "strict":
				_ = inv.Policy.SetPreset(inv.SessionID, preset, inv.Emit)
				if err := inv.Policy.SetSandboxMode(inv.SessionID, session.SandboxReadOnly, "", inv.Emit); err != nil {
					return CommandResult{Kind: "error", Text: err.Error()}
				}
				_ = inv.Policy.SetApprovalPolicy(inv.SessionID, session.ApprovalPolicyNever, "", inv.Emit)
			case "unrestricted":
				_ = inv.Policy.SetPreset(inv.SessionID, preset, inv.Emit)
				if err := inv.Policy.SetSandboxMode(inv.SessionID, session.SandboxDangerFullAccess, "", inv.Emit); err != nil {
					return CommandResult{Kind: "error", Text: err.Error()}
				}
				_ = inv.Policy.SetApprovalPolicy(inv.SessionID, session.ApprovalPolicyAsk, "", inv.Emit)
			default:
				return CommandResult{Kind: "error", Text: "unknown preset; use /permission default | strict | unrestricted"}
			}
			return CommandResult{Kind: "success", Text: fmt.Sprintf("Permission preset %s applied.", preset)}
		},
	})

	cr.Register(CommandDefinition{
		Name:        "feedback",
		Description: "Record feedback about this session; /feedback <text> appends one log-only feedback/record event.",
		Handler: func(inv CommandInvocation) CommandResult {
			text := strings.TrimSpace(inv.RawInput)
			if text == "" {
				return CommandResult{Kind: "error", Text: "Feedback text is required. Usage: /feedback <text>"}
			}
			if inv.Emit != nil {
				inv.Emit(session.EventFeedbackRecord, session.FeedbackRecordPayload{Text: text})
			}
			return CommandResult{Kind: "success", Text: fmt.Sprintf("Feedback recorded for session %s", inv.SessionID)}
		},
	})
	cr.Register(CommandDefinition{
		Name:        "exit",
		Description: "Exit the interactive session.",
		Handler: func(inv CommandInvocation) CommandResult {
			return CommandResult{Kind: "success", Text: "exit"}
		},
	})

	cr.Register(CommandDefinition{
		Name:        "help",
		Description: "List available commands.",
		Handler: func(inv CommandInvocation) CommandResult {
			cr.mu.RLock()
			defer cr.mu.RUnlock()
			var b strings.Builder
			for _, def := range cr.cmds {
				b.WriteString(fmt.Sprintf("/%s — %s\n", def.Name, def.Description))
			}
			return CommandResult{Kind: "success", Text: b.String()}
		},
	})
}
