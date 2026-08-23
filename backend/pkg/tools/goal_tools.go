package tools

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"dsh-go/pkg/session"
)

// GoalState is the durable same-session goal mirror (upstream tool-goal:
// id/revision/objective/phase/roundsStarted/maxGoalRounds/blockedReason +
// activation).
type GoalState struct {
	ID            string
	Revision      int
	Objective     string
	Phase         string // "active" | "paused" | "blocked" | "complete"
	RoundsStarted int
	MaxGoalRounds int
	BlockedReason *session.GoalBlockReason
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// goalToolView is the canonical model-facing output shape.
type goalToolView struct {
	Goal       *goalView `json:"goal"`
	Activation string    `json:"activation"`
}

type goalView struct {
	ID            string                   `json:"id"`
	Revision      int                      `json:"revision"`
	Objective     string                   `json:"objective"`
	Phase         string                   `json:"phase"`
	RoundsStarted int                      `json:"roundsStarted"`
	MaxGoalRounds int                      `json:"maxGoalRounds"`
	BlockedReason *session.GoalBlockReason `json:"blockedReason,omitempty"`
}

var (
	goalMu    sync.RWMutex
	goalsByID = map[string]*GoalState{} // session id -> goal
	goalSeq   = 0
)

func currentGoal(sessionID string) *GoalState {
	goalMu.RLock()
	defer goalMu.RUnlock()
	return goalsByID[sessionID]
}

// goalEmitChange emits the durable goal/change event through the agent
// (nil emit disables durability in direct tests).
func goalEmitChange(ctx ToolExecutionContext, g *GoalState, operation string) {
	if ctx.Emit == nil {
		return
	}
	ctx.Emit(session.EventGoalChange, session.GoalChangePayload{
		Kind:      "goal/change",
		Version:   1,
		Operation: operation,
		Goal: &session.GoalSnapshot{
			ID:            g.ID,
			Revision:      g.Revision,
			Objective:     g.Objective,
			Phase:         g.Phase,
			BlockedReason: g.BlockedReason,
			MaxGoalRounds: g.MaxGoalRounds,
		},
		RoundsStarted: g.RoundsStarted,
		CreatedAt:     g.CreatedAt.UnixMilli(),
		UpdatedAt:     g.UpdatedAt.UnixMilli(),
	})
}

func goalViewOf(g *GoalState) goalToolView {
	if g == nil {
		return goalToolView{Goal: nil, Activation: "disarmed"}
	}
	return goalToolView{
		Goal: &goalView{
			ID:            g.ID,
			Revision:      g.Revision,
			Objective:     g.Objective,
			Phase:         g.Phase,
			RoundsStarted: g.RoundsStarted,
			MaxGoalRounds: g.MaxGoalRounds,
			BlockedReason: g.BlockedReason,
		},
		Activation: "armed",
	}
}

// RegisterGoalTools registers get_goal, create_goal and update_goal (upstream
// @deepseek-ai/dsh-tool-goal contract).
func (r *ToolRegistry) RegisterGoalTools() {
	r.Register(ToolDefinition{
		Name:        "get_goal",
		Description: "Read the current same-session goal, including its exact id/revision, objective, phase, completed continuation rounds, round limit, blocker reason when present, and whether another continuation is armed. Call this before updating a goal.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {},
			"required": []
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			return goalViewOf(currentGoal(ctx.SessionID)), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "create_goal",
		Description: "Create one persisted same-session completion goal when the current direct human request is a long-running objective that should continue across autonomous goal rounds.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"objective": { "type": "string", "description": "The concrete completion objective inferred from the direct human request" },
				"max_goal_rounds": { "type": "integer", "description": "Optional limit on automatic continuation rounds" }
			},
			"required": ["objective"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Objective     string `json:"objective"`
				MaxGoalRounds int    `json:"max_goal_rounds"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if args.Objective == "" {
				return nil, fmt.Errorf("objective must be a non-empty string")
			}
			goalMu.Lock()
			goalSeq++
			now := time.Now()
			g := &GoalState{
				ID:            fmt.Sprintf("goal-%d", goalSeq),
				Revision:      1,
				Objective:     args.Objective,
				Phase:         "active",
				MaxGoalRounds: args.MaxGoalRounds,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			goalsByID[ctx.SessionID] = g
			goalMu.Unlock()
			goalEmitChange(ctx, g, "create")
			return goalViewOf(g), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "update_goal",
		Description: "Update the exact current goal revision. edit, pause, and resume require a direct top-level human request. complete and blocked are allowed during an automatic continuation.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"goal_id": { "type": "string", "description": "Exact id returned by get_goal" },
				"revision": { "type": "integer", "description": "Exact revision returned by get_goal" },
				"action": { "type": "string", "enum": ["edit", "pause", "resume", "complete", "blocked"], "description": "Goal lifecycle action" },
				"objective": { "type": "string", "description": "New objective (edit only)" },
				"max_goal_rounds": { "type": "integer", "description": "New round cap (edit only)" },
				"blocked_reason": { "type": "string", "description": "Concrete blocking condition; required only with action blocked" }
			},
			"required": ["goal_id", "revision", "action"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				GoalID        string `json:"goal_id"`
				Revision      int    `json:"revision"`
				Action        string `json:"action"`
				Objective     string `json:"objective"`
				MaxGoalRounds int    `json:"max_goal_rounds"`
				BlockedReason string `json:"blocked_reason"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			g := currentGoal(ctx.SessionID)
			if g == nil {
				return nil, fmt.Errorf("no goal in this session; create one with create_goal first")
			}
			if g.ID != args.GoalID || g.Revision != args.Revision {
				return nil, fmt.Errorf("goal id/revision mismatch; call get_goal for the exact current values")
			}
			op := args.Action
			switch args.Action {
			case "edit":
				if args.Objective != "" {
					g.Objective = args.Objective
				}
				if args.MaxGoalRounds > 0 {
					g.MaxGoalRounds = args.MaxGoalRounds
				}
			case "pause":
				g.Phase = "paused"
			case "resume":
				g.Phase = "active"
			case "complete":
				g.Phase = "complete"
			case "blocked":
				if args.BlockedReason == "" {
					return nil, fmt.Errorf("blocked_reason is required for action blocked")
				}
				g.Phase = "blocked"
				g.BlockedReason = &session.GoalBlockReason{Code: "blocked", Message: args.BlockedReason}
			default:
				return nil, fmt.Errorf("unknown goal action %q", args.Action)
			}
			g.Revision++
			g.UpdatedAt = time.Now()
			goalEmitChange(ctx, g, op)
			return goalViewOf(g), nil
		},
	})
}
