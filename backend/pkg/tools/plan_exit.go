package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"dsh-go/pkg/session"
)

// exitPlanModeArgs mirrors the upstream exit_plan_mode schema: the complete
// markdown plan, starting with a # heading that names it.
type exitPlanModeArgs struct {
	Plan string `json:"plan"`
}

// RegisterExitPlanModeTool registers exit_plan_mode (upstream
// @deepseek-ai/dsh-plan-mode). The tool stays registered while plan mode is
// inactive so the request tool catalog is stable across transitions; its
// execution validates the folded plan state and fails closed outside plan
// mode. On approval the tool appends the plan/mode false event, so the next
// model step runs with the default mode.
func (r *ToolRegistry) RegisterExitPlanModeTool() {
	r.Register(ToolDefinition{
		Name: "exit_plan_mode",
		Description: "Use only in plan mode. Present your plan for the user's review and, on approval, leave plan mode. " +
			"Send the COMPLETE plan as markdown, starting with a # heading that names it. " +
			"The user may approve (carry out the plan from your next step) or keep planning; their feedback comes back in the tool result; revise and present again.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"plan": { "type": "string", "description": "The complete plan, as markdown, starting with a # heading that names it" }
			},
			"required": ["plan"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args exitPlanModeArgs
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if !sessionPlanMode(ctx.SessionID) {
				return nil, fmt.Errorf("exit_plan_mode is only available in plan mode")
			}
			if !strings.HasPrefix(strings.TrimSpace(args.Plan), "# ") {
				return nil, fmt.Errorf("exit_plan_mode requires a non-empty markdown plan starting with a # heading")
			}
			// The plan review is a human decision; without an answerer the
			// call fails closed (upstream: no user-questions channel fails).
			if ctx.RequestUser == nil {
				return nil, fmt.Errorf("no user-questions channel is available to review the plan; ask the user to switch the session mode instead")
			}
			decision, err := ctx.RequestUser(
				"Approve this plan and leave plan mode?\n\n"+args.Plan,
				[]string{"Approve", "Keep planning"},
			)
			if err != nil {
				return nil, fmt.Errorf("plan review failed: %w", err)
			}
			switch decision {
			case ApprovalAllowOnce:
				// Approved: leave plan mode. The event is the authoritative
				// transition (upstream appends plan/mode {active:false});
				// the mirror keeps later reads consistent until the log
				// replay catches up.
				setSessionPlanMode(ctx.SessionID, false)
				if ctx.Emit != nil {
					ctx.Emit(session.EventPlanMode, session.PlanModePayload{Active: false})
				}
				return map[string]any{"approved": true}, nil
			case ApprovalDeny:
				return nil, fmt.Errorf("the user chose to keep planning; revise the plan and present it again")
			default:
				return nil, fmt.Errorf("the user dismissed the plan review to speak instead; stay in plan mode, stop here, and wait for their message")
			}
		},
	})
}
