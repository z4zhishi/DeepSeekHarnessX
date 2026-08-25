package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"dsh-go/pkg/session"
)

// TodoItem represents a structured task in DSH todo/write.
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // "pending" | "in_progress" | "completed"
}

// Todo statuses (upstream tool-todo STATUSES: the model-facing enum starts at
// "pending"; "todo" is only tolerated when decoding legacy logs).
const (
	TodoStatusPending    = "pending"
	TodoStatusInProgress = "in_progress"
	TodoStatusCompleted  = "completed"
)

// todoStatusLegacy maps retired enum spellings onto the canonical ones so old
// session logs / persisted snapshots replay instead of failing schema decode.
var todoStatusLegacy = map[string]string{
	"todo": TodoStatusPending,
}

// normalizeTodoStatus validates one status and folds legacy spellings.
func normalizeTodoStatus(status string) (string, error) {
	if mapped, ok := todoStatusLegacy[status]; ok {
		return mapped, nil
	}
	switch status {
	case TodoStatusPending, TodoStatusInProgress, TodoStatusCompleted:
		return status, nil
	default:
		return "", fmt.Errorf("invalid todo status %q: must be one of pending|in_progress|completed", status)
	}
}

// toTodoList mirrors upstream toTodoList (tool-todo/src/index.ts:91): trimmed
// non-empty unique content and at most one in_progress item. The registry has
// already enforced the wire enum; this pass also folds the legacy "todo"
// spelling to "pending".
func toTodoList(raw []TodoItem) ([]TodoItem, error) {
	todos := make([]TodoItem, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	active := 0
	for _, item := range raw {
		content := strings.TrimSpace(item.Content)
		if len(content) == 0 {
			return nil, fmt.Errorf("invalid todo: `content` must be a non-empty string")
		}
		if seen[content] {
			return nil, fmt.Errorf("invalid todos: duplicate content %q", content)
		}
		seen[content] = true
		status, err := normalizeTodoStatus(item.Status)
		if err != nil {
			return nil, err
		}
		if status == TodoStatusInProgress {
			active++
		}
		todos = append(todos, TodoItem{Content: content, Status: status})
	}
	if active > 1 {
		return nil, fmt.Errorf("invalid todos: at most one task may be in_progress (got %d)", active)
	}
	return todos, nil
}

// TaskStore holds in-memory structured state for todos, plans, and goals.
type TaskStore struct {
	todos       map[string][]TodoItem
	plans       map[string]string
	plansActive map[string]bool
	goals       map[string]string
	mu          sync.RWMutex
}

var globalTasks = &TaskStore{
	todos:       make(map[string][]TodoItem),
	plans:       make(map[string]string),
	plansActive: make(map[string]bool),
	goals:       make(map[string]string),
}

// setSessionPlan keeps the live plan mirror for a session (the plan/mode
// event is the durable source; the mirror serves the /plan command handler
// and later model turns).
func setSessionPlan(sessionID, plan string) {
	globalTasks.mu.Lock()
	defer globalTasks.mu.Unlock()
	globalTasks.plans[sessionID] = plan
}

// setSessionPlanMode keeps the live plan-mode mirror for a session (the
// plan/mode event is the durable source; the mirror serves exit_plan_mode
// between agent turns, exactly like the plan-text mirror).
func setSessionPlanMode(sessionID string, active bool) {
	globalTasks.mu.Lock()
	defer globalTasks.mu.Unlock()
	globalTasks.plansActive[sessionID] = active
}

// sessionPlanMode reads the live plan-mode mirror (last selection wins;
// a session with no selection is inactive).
func sessionPlanMode(sessionID string) bool {
	globalTasks.mu.RLock()
	defer globalTasks.mu.RUnlock()
	return globalTasks.plansActive[sessionID]
}

// RegisterTaskTools registers todo_write, plan_mode, and goal_tracker tools.
func (r *ToolRegistry) RegisterTaskTools() {
	// 1. todo_write
	r.Register(ToolDefinition{
		Name:        "todo_write",
		Description: "Update the structured todo list for the active task.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"todos": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"content": { "type": "string" },
							"status": { "type": "string", "enum": ["pending", "in_progress", "completed"] }
						},
						"required": ["content", "status"]
					}
				}
			},
			"required": ["todos"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Todos []TodoItem `json:"todos"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			// Canonical list validation (upstream toTodoList): non-empty
			// unique content, known status, single in_progress.
			todos, err := toTodoList(args.Todos)
			if err != nil {
				return nil, err
			}

			globalTasks.mu.Lock()
			globalTasks.todos[ctx.SessionID] = todos
			globalTasks.mu.Unlock()

			// Durable todo snapshot (upstream todo/write, whole-list replace).
			if ctx.Emit != nil {
				items := make([]session.TodoItem, len(todos))
				for i, it := range todos {
					items[i] = session.TodoItem{Content: it.Content, Status: it.Status}
				}
				ctx.Emit(session.EventTodoWrite, session.TodoWritePayload{Todos: items})
			}

			return fmt.Sprintf("Updated %d todo items.", len(args.Todos)), nil
		},
	})

	// 2. plan_update
	r.Register(ToolDefinition{
		Name:        "plan_update",
		Description: "Set or update the active technical implementation plan.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"plan": { "type": "string", "description": "Markdown formatted plan content" }
			},
			"required": ["plan"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Plan string `json:"plan"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}

			globalTasks.mu.Lock()
			globalTasks.plans[ctx.SessionID] = args.Plan
			globalTasks.plansActive[ctx.SessionID] = true
			globalTasks.mu.Unlock()

			// Durable plan-mode snapshot (upstream plan/mode, whole-value replace).
			if ctx.Emit != nil {
				ctx.Emit(session.EventPlanMode, session.PlanModePayload{Active: true})
			}

			return "Implementation plan recorded successfully.", nil
		},
	})

	// 3. goal_set
	r.Register(ToolDefinition{
		Name:        "goal_set",
		Description: "Define or update the main objective goal of the session.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"goal": { "type": "string", "description": "Objective statement" }
			},
			"required": ["goal"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Goal string `json:"goal"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}

			globalTasks.mu.Lock()
			globalTasks.goals[ctx.SessionID] = args.Goal
			globalTasks.mu.Unlock()

			// Durable goal snapshot (upstream goal/change, create operation).
			if ctx.Emit != nil {
				now := time.Now().UnixMilli()
				ctx.Emit(session.EventGoalChange, session.GoalChangePayload{
					Kind:      "goal/change",
					Version:   1,
					Operation: "create",
					Goal: &session.GoalSnapshot{
						ID:            fmt.Sprintf("goal-%d", now),
						Revision:      1,
						Objective:     args.Goal,
						Phase:         "active",
						MaxGoalRounds: 0,
					},
					RoundsStarted: 0,
					CreatedAt:     now,
					UpdatedAt:     now,
				})
			}

			return fmt.Sprintf("Active goal set to: %s", args.Goal), nil
		},
	})
}
