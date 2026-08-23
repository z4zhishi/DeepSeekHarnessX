package tools

import (
	"encoding/json"
	"fmt"

	"dsh-go/pkg/session"
)

// RegisterPolicyTools registers model/user-facing policy-knob tools. The
// session log events (sandbox/mode, approval/policy, permission/preset) are
// the durable source; the PolicyStore mirror keeps enforcement live. These
// mirror the upstream /permission command's knob semantics without requiring
// a slash-command registry.
func (r *ToolRegistry) RegisterPolicyTools() {
	r.Register(ToolDefinition{
		Name:        "set_sandbox_mode",
		Description: "Switch this session's file sandbox mode: read-only | workspace-write | danger-full-access. The last switch wins; the change is durable and replayable.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"mode": { "type": "string", "enum": ["read-only", "workspace-write", "danger-full-access"] }
			},
			"required": ["mode"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Mode session.SandboxMode `json:"mode"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if r.Policy == nil {
				return nil, fmt.Errorf("policy store unavailable")
			}
			if err := r.Policy.SetSandboxMode(ctx.SessionID, args.Mode, "", ctx.Emit); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Sandbox mode set to %s.", args.Mode), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "set_approval_policy",
		Description: "Switch the session's approval policy: ask (prompt the user) | never (reject every approval-required action deterministically).",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"policy": { "type": "string", "enum": ["ask", "never"] }
			},
			"required": ["policy"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Policy session.ApprovalPolicy `json:"policy"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if r.Policy == nil {
				return nil, fmt.Errorf("policy store unavailable")
			}
			if err := r.Policy.SetApprovalPolicy(ctx.SessionID, args.Policy, "", ctx.Emit); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Approval policy set to %s.", args.Policy), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "set_permission_preset",
		Description: "Record a named permission preset selection (durable user intent; the sandbox/approval knob events follow in the same turn).",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"preset": { "type": "string", "description": "Preset name such as default | strict | unrestricted" }
			},
			"required": ["preset"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Preset string `json:"preset"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if r.Policy == nil {
				return nil, fmt.Errorf("policy store unavailable")
			}
			if err := r.Policy.SetPreset(ctx.SessionID, args.Preset, ctx.Emit); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Permission preset set to %s.", args.Preset), nil
		},
	})
}
