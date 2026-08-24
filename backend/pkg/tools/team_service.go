package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dsh-go/pkg/session"
)

// TeamService implements the Agent Teams runtime (upstream
// packages/experimental/agent-team): durable roster, shared task board, and
// peer mailbox over the Team Lead session log, plus live child agents.
//
// The four Team events (team/member, team/task, team/message/queued,
// team/message/delivered) are the authoritative source; every operation
// folds the Lead log, mutates, and appends through the agent's Emit path.

// TeamChild is one live teammate session handle owned by the host.
type TeamChild interface {
	ID() string
	PostUserMessage(msg session.UserMessagePayload)
	Interrupt()
	Status() string // "running" | "idle" | "inactive" | "provisioning" | "failed"
	Model() string
	Stop()
}

// TeamSpawner creates one teammate child; the host wires it to a real agent.
type TeamSpawner func(name, description, prompt, context string) (TeamChild, error)

// TeamService owns one process-local Team runtime per lead session.
type TeamService struct {
	mu      sync.RWMutex
	leads   map[string]*teamRuntime // lead session id -> runtime
	spawner TeamSpawner
	now     func() time.Time
}

type teamRuntime struct {
	leadID  string
	spawner TeamSpawner
	// live children: child id -> handle
	children map[string]TeamChild
	// watch: wait_agent subscribers
	watchers map[chan struct{}]bool
	rev      chan struct{} // broadcast on any team change
}

var globalTeam = &TeamService{
	leads: map[string]*teamRuntime{},
	now:   time.Now,
}

// SetTeamSpawner installs the host spawner (gateway/main wire their agent
// factory here).
func SetTeamSpawner(s TeamSpawner) {
	globalTeam.mu.Lock()
	defer globalTeam.mu.Unlock()
	globalTeam.spawner = s
}

// resetTeamRuntime clears per-lead runtime state (tests).
func resetTeamRuntime(leadID string) {
	globalTeam.mu.Lock()
	defer globalTeam.mu.Unlock()
	delete(globalTeam.leads, leadID)
}

// teamEventsFor folds Team events from the lead session log.
func teamEventsFor(ctx ToolExecutionContext) ([]*session.SessionEnvelope, error) {
	var events []*session.SessionEnvelope
	if ctx.Events != nil {
		events = ctx.Events()
	} else {
		events = eventsForSession(ctx.SessionID)
	}
	return events, nil
}

// ===== Roster =====

// teamMembership resolves one caller's role in a team.
type teamMembership struct {
	rootID string
	role   string // "lead" | "teammate"
	name   string
}

func (ts *TeamService) membership(leadID, callerID string) teamMembership {
	if callerID == leadID {
		return teamMembership{rootID: leadID, role: "lead", name: "lead"}
	}
	return teamMembership{rootID: leadID, role: "teammate", name: callerID}
}

// ===== Task board =====

// teamTaskCreate creates one unowned pending task.
func (ts *TeamService) teamTaskCreate(ctx ToolExecutionContext, subject, description string, blockedBy, writeScopes []string) (teamTaskView, error) {
	if strings.TrimSpace(subject) == "" {
		return teamTaskView{}, fmt.Errorf("subject must be non-empty")
	}
	if strings.TrimSpace(description) == "" {
		return teamTaskView{}, fmt.Errorf("description must be non-empty")
	}
	if len(subject) > 200 {
		return teamTaskView{}, fmt.Errorf("subject too long")
	}
	if len(description) > 16384 {
		return teamTaskView{}, fmt.Errorf("description too long")
	}
	events, err := teamEventsFor(ctx)
	if err != nil {
		return teamTaskView{}, err
	}
	state, err := foldTeamEvents(events, ctx.SessionID)
	if err != nil {
		return teamTaskView{}, err
	}
	task := session.TeamTaskRecord{
		ID:          fmt.Sprintf("task-%d", state.nextTaskID),
		Revision:    1,
		Subject:     subject,
		Description: description,
		Status:      "pending",
		BlockedBy:   blockedBy,
		WriteScopes: writeScopes,
	}
	if err := validateTaskGraph(state.tasks, task); err != nil {
		return teamTaskView{}, err
	}
	if ctx.Emit != nil {
		ctx.Emit(session.EventTeamTask, session.TeamTaskPayload{
			Version: teamEventVersion,
			TeamID:  ctx.SessionID,
			Task:    task,
		})
	}
	state.tasks[task.ID] = task
	return teamTaskViewOf(state, task), nil
}

// teamTaskList returns non-deleted tasks in numeric creation order.
func (ts *TeamService) teamTaskList(ctx ToolExecutionContext, status string) ([]teamTaskView, error) {
	events, err := teamEventsFor(ctx)
	if err != nil {
		return nil, err
	}
	state, err := foldTeamEvents(events, ctx.SessionID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.tasks))
	for id := range state.tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return taskNum(ids[i]) < taskNum(ids[j])
	})
	var out []teamTaskView
	for _, id := range ids {
		t := state.tasks[id]
		if t.Status == "deleted" {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		out = append(out, teamTaskViewOf(state, t))
	}
	return out, nil
}

// teamTaskGet returns one task by id.
func (ts *TeamService) teamTaskGet(ctx ToolExecutionContext, id string) (teamTaskView, error) {
	events, err := teamEventsFor(ctx)
	if err != nil {
		return teamTaskView{}, err
	}
	state, err := foldTeamEvents(events, ctx.SessionID)
	if err != nil {
		return teamTaskView{}, err
	}
	t, ok := state.tasks[id]
	if !ok {
		return teamTaskView{}, fmt.Errorf("team task %q not found", id)
	}
	return teamTaskViewOf(state, t), nil
}

// teamTaskUpdate applies one compare-and-set task transition.
func (ts *TeamService) teamTaskUpdate(ctx ToolExecutionContext, id string, expectedRev int, action string, fields map[string]any) (teamTaskView, error) {
	events, err := teamEventsFor(ctx)
	if err != nil {
		return teamTaskView{}, err
	}
	state, err := foldTeamEvents(events, ctx.SessionID)
	if err != nil {
		return teamTaskView{}, err
	}
	current, ok := state.tasks[id]
	if !ok {
		return teamTaskView{}, fmt.Errorf("team task %q not found", id)
	}
	if current.Revision != expectedRev {
		return teamTaskView{}, fmt.Errorf("revision mismatch: expected %d, got %d", expectedRev, current.Revision)
	}
	next := current
	switch action {
	case "claim":
		if current.Status != "pending" {
			return teamTaskView{}, fmt.Errorf("only a pending task can be claimed")
		}
		next.Status = "in_progress"
		next.OwnerID = callerID(ctx)
	case "release":
		if current.Status != "in_progress" || current.OwnerID != callerID(ctx) {
			return teamTaskView{}, fmt.Errorf("only the owning member can release a claimed task")
		}
		next.Status = "pending"
		next.OwnerID = ""
	case "edit":
		if s, ok := fields["subject"].(string); ok {
			if strings.TrimSpace(s) == "" {
				return teamTaskView{}, fmt.Errorf("subject must be non-empty")
			}
			next.Subject = s
		}
		if d, ok := fields["description"].(string); ok {
			if strings.TrimSpace(d) == "" {
				return teamTaskView{}, fmt.Errorf("description must be non-empty")
			}
			next.Description = d
		}
		if raw, ok := fields["write_scopes"]; ok {
			var scopes []string
			for _, v := range raw.([]any) {
				scopes = append(scopes, v.(string))
			}
			next.WriteScopes = scopes
		}
	case "set_dependencies":
		var deps []string
		if raw, ok := fields["blocked_by"]; ok {
			for _, v := range raw.([]any) {
				deps = append(deps, v.(string))
			}
		}
		if err := validateTaskGraph(state.tasks, session.TeamTaskRecord{ID: current.ID, BlockedBy: deps}); err != nil {
			return teamTaskView{}, err
		}
		next.BlockedBy = deps
	case "complete":
		if current.Status != "in_progress" {
			return teamTaskView{}, fmt.Errorf("only an in-progress task can be completed")
		}
		if !taskReady(state, current) {
			return teamTaskView{}, fmt.Errorf("task is blocked")
		}
		next.Status = "completed"
	case "reopen":
		if current.Status != "completed" {
			return teamTaskView{}, fmt.Errorf("only a completed task can reopen")
		}
		next.Status = "pending"
		next.OwnerID = ""
	case "reassign":
		if current.Status != "pending" && current.Status != "in_progress" {
			return teamTaskView{}, fmt.Errorf("only a pending or in-progress task can be reassigned")
		}
		owner, _ := fields["owner"].(string)
		if strings.TrimSpace(owner) == "" {
			next.Status = "pending"
			next.OwnerID = ""
		} else {
			// resolve member name
			name := strings.TrimSpace(owner)
			if name != "lead" {
				member, ok := state.members[name]
				if !ok {
					return teamTaskView{}, fmt.Errorf("active teammate %q not found", name)
				}
				next.OwnerID = member.ID
			} else {
				next.OwnerID = ctx.SessionID
			}
			next.Status = "in_progress"
		}
	case "delete":
		for _, other := range state.tasks {
			if other.Status != "deleted" && other.ID != current.ID {
				for _, b := range other.BlockedBy {
					if b == current.ID {
						return teamTaskView{}, fmt.Errorf("task %q still blocks %q", current.ID, other.ID)
					}
				}
			}
		}
		next.Status = "deleted"
	default:
		return teamTaskView{}, fmt.Errorf("unsupported task action %q", action)
	}
	next.Revision = current.Revision + 1
	if err := validateTaskGraph(state.tasks, next); err != nil {
		return teamTaskView{}, err
	}
	if ctx.Emit != nil {
		ctx.Emit(session.EventTeamTask, session.TeamTaskPayload{
			Version: teamEventVersion,
			TeamID:  ctx.SessionID,
			Task:    next,
		})
	}
	return teamTaskViewOf(state, next), nil
}

// callerID resolves the executing agent's identity. The shared pipeline never
// populates CallerID (loop.go omits it), so a single-lead deployment collapses
// to the lead session id; a host that populates CallerID for teammates gets
// per-member identity.
func callerID(ctx ToolExecutionContext) string {
	if ctx.CallerID != "" {
		return ctx.CallerID
	}
	return ctx.SessionID
}

// toAnySlice converts a string slice to the []any the task-update field
// decoder expects (the fold path reads blocked_by / write_scopes via
// raw.([]any)).
func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// resolveTeamTarget maps a member name (or "lead") to its session id.
func resolveTeamTarget(state *teamFoldState, leadID, raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "lead" {
		return leadID, nil
	}
	if m, ok := state.members[name]; ok && m.Phase == "active" {
		return m.ID, nil
	}
	return "", fmt.Errorf("active teammate %q not found", name)
}

func taskNum(id string) int {
	if m := taskIDRe.FindStringSubmatch(id); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func taskReady(state *teamFoldState, t session.TeamTaskRecord) bool {
	for _, b := range t.BlockedBy {
		if state.tasks[b].Status != "completed" {
			return false
		}
	}
	return true
}

// ===== Mailbox =====

// teamMessageSend queues one durable peer message and attempts delivery.
func (ts *TeamService) teamMessageSend(ctx ToolExecutionContext, target, text, delivery string) (map[string]any, error) {
	events, err := teamEventsFor(ctx)
	if err != nil {
		return nil, err
	}
	state, err := foldTeamEvents(events, ctx.SessionID)
	if err != nil {
		return nil, err
	}
	targetID, err := resolveTeamTarget(state, ctx.SessionID, target)
	if err != nil {
		return nil, err
	}
	msg := session.TeamMessageRecord{
		ID:         fmt.Sprintf("msg-%d", time.Now().UnixNano()),
		SenderID:   ctx.CallerID,
		SenderName: ctx.CallerName,
		TargetID:   targetID,
		Delivery:   delivery,
		Content:    []session.ContentBlock{{Type: "text", Text: text}},
	}
	if ctx.Emit != nil {
		ctx.Emit(session.EventTeamMessageQueued, session.TeamMessagePayload{
			Version: teamEventVersion,
			TeamID:  ctx.SessionID,
			Message: msg,
		})
	}
	// Deliver to a live child (or the lead itself) synchronously; cold resume
	// of an inactive target is host-driven.
	accepted := false
	ts.mu.Lock()
	rt := ts.leads[ctx.SessionID]
	if rt != nil {
		var child TeamChild
		if msg.TargetID == ctx.SessionID {
			child = nil // lead delivery is the caller's own loop
		} else {
			child = rt.children[msg.TargetID]
		}
		if child != nil {
			child.PostUserMessage(session.UserMessagePayload{
				ID:      fmt.Sprintf("team-%s", msg.ID),
				Role:    "user",
				Content: []session.ContentBlock{{Type: "text", Text: fmt.Sprintf("Team message %s from %s:\n%s", msg.ID, msg.SenderName, text)}},
				Source:  session.MessageSource{Kind: "plugin", Plugin: "team"},
			})
			accepted = true
		}
	}
	ts.mu.Unlock()
	status := "queued"
	if accepted {
		status = "accepted"
	}
	return map[string]any{"messageId": msg.ID, "status": status}, nil
}

// ===== roster views =====

// teamListMembers lists the lead and every active teammate.
func (ts *TeamService) teamListMembers(ctx ToolExecutionContext) ([]teamMemberView, error) {
	events, err := teamEventsFor(ctx)
	if err != nil {
		return nil, err
	}
	state, err := foldTeamEvents(events, ctx.SessionID)
	if err != nil {
		return nil, err
	}
	ts.mu.Lock()
	rt := ts.leads[ctx.SessionID]
	ts.mu.Unlock()
	lead := teamMemberView{
		ID:          ctx.SessionID,
		Name:        "lead",
		Role:        "lead",
		Status:      "running",
		Diagnostics: []string{},
	}
	out := []teamMemberView{lead}
	for _, m := range state.members {
		status := "inactive"
		if rt != nil {
			if _, live := rt.children[m.ID]; live {
				status = "running"
			}
		}
		if m.Phase == "failed" {
			status = "failed"
		}
		out = append(out, teamMemberView{
			ID:          m.ID,
			Name:        m.Name,
			Role:        "teammate",
			Status:      status,
			Description: m.Description,
			Provider:    m.Provider,
			Context:     m.Context,
			Diagnostics: []string{},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// teamWait waits for one team edge up to timeoutMs.
func (ts *TeamService) teamWait(ctx ToolExecutionContext, timeoutMs int) (map[string]any, error) {
	ts.mu.Lock()
	rt := ts.leads[ctx.SessionID]
	if rt == nil {
		ts.mu.Unlock()
		return map[string]any{"timedOut": true, "noProgress": map[string]any{"reason": "no-active-peer", "message": "No other Team member is running or provisioning."}}, nil
	}
	ch := make(chan struct{}, 1)
	rt.watchers[ch] = true
	ts.mu.Unlock()
	timeout := time.After(time.Duration(timeoutMs) * time.Millisecond)
	select {
	case <-ch:
	case <-timeout:
	}
	ts.mu.Lock()
	delete(rt.watchers, ch)
	ts.mu.Unlock()
	return map[string]any{"timedOut": false}, nil
}

func (ts *TeamService) notifyChange(leadID string) {
	ts.mu.Lock()
	rt := ts.leads[leadID]
	if rt == nil {
		ts.mu.Unlock()
		return
	}
	for ch := range rt.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	ts.mu.Unlock()
}

// ===== runtime lifecycle =====

// registerLead registers the lead runtime for one session.
func (ts *TeamService) registerLead(leadID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if _, ok := ts.leads[leadID]; !ok {
		ts.leads[leadID] = &teamRuntime{
			leadID:   leadID,
			spawner:  ts.spawner,
			children: map[string]TeamChild{},
			watchers: map[chan struct{}]bool{},
		}
	}
}

// unregisterLead tears down the lead runtime.
func (ts *TeamService) unregisterLead(leadID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if rt, ok := ts.leads[leadID]; ok {
		for _, c := range rt.children {
			c.Stop()
		}
		delete(ts.leads, leadID)
	}
}

// spawnTeammate creates a durable teammate through the host spawner.
func (ts *TeamService) spawnTeammate(ctx ToolExecutionContext, name, description, prompt, context string) (teamMemberView, error) {
	if !memberNameRe.MatchString(name) || len(name) > teamMemberNameMax || name == "lead" {
		return teamMemberView{}, fmt.Errorf("teammate name must be lower-kebab-case, at most %d characters, and not \"lead\"", teamMemberNameMax)
	}
	if strings.TrimSpace(description) == "" {
		return teamMemberView{}, fmt.Errorf("description must be non-empty")
	}
	ts.mu.Lock()
	spawner := ts.spawner
	rt := ts.leads[ctx.SessionID]
	ts.mu.Unlock()
	if spawner == nil {
		return teamMemberView{}, fmt.Errorf("no teammate provider is configured in this host")
	}
	// Member name uniqueness and limit from the durable fold.
	events, err := teamEventsFor(ctx)
	if err != nil {
		return teamMemberView{}, err
	}
	state, err := foldTeamEvents(events, ctx.SessionID)
	if err != nil {
		return teamMemberView{}, err
	}
	if _, ok := state.memberNames[name]; ok {
		return teamMemberView{}, fmt.Errorf("teammate name %q was already used in this Team", name)
	}
	if len(state.members) >= 16 {
		return teamMemberView{}, fmt.Errorf("Team member limit 16 reached")
	}
	child, err := spawner(name, description, prompt, context)
	if err != nil {
		return teamMemberView{}, err
	}
	member := session.TeamMemberRecord{
		ID:          child.ID(),
		Name:        name,
		Description: description,
		Provider:    "spawn",
		Context:     context,
		Phase:       "provisioning",
	}
	if ctx.Emit != nil {
		ctx.Emit(session.EventTeamMember, session.TeamMemberPayload{Version: teamEventVersion, TeamID: ctx.SessionID, Member: member})
	}
	member.Phase = "active"
	if ctx.Emit != nil {
		ctx.Emit(session.EventTeamMember, session.TeamMemberPayload{Version: teamEventVersion, TeamID: ctx.SessionID, Member: member})
	}
	ts.mu.Lock()
	if rt != nil {
		rt.children[member.ID] = child
	}
	ts.mu.Unlock()
	ts.notifyChange(ctx.SessionID)
	return teamMemberView{
		ID:          member.ID,
		Name:        member.Name,
		Role:        "teammate",
		Status:      "running",
		Description: member.Description,
		Provider:    member.Provider,
		Context:     member.Context,
		Diagnostics: []string{},
	}, nil
}

// interruptAgent interrupts one teammate turn (lead only).
func (ts *TeamService) interruptAgent(ctx ToolExecutionContext, target string) (map[string]any, error) {
	events, err := teamEventsFor(ctx)
	if err != nil {
		return nil, err
	}
	state, err := foldTeamEvents(events, ctx.SessionID)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(target)
	ts.mu.Lock()
	rt := ts.leads[ctx.SessionID]
	var child TeamChild
	if name == "lead" {
		// lead interrupts itself (no-op for the caller)
		ts.mu.Unlock()
		return map[string]any{"target": "lead", "previousStatus": "running"}, nil
	}
	member, ok := state.members[name]
	if !ok {
		ts.mu.Unlock()
		return nil, fmt.Errorf("active teammate %q not found", name)
	}
	if rt != nil {
		child = rt.children[member.ID]
	}
	ts.mu.Unlock()
	if child == nil {
		return nil, fmt.Errorf("teammate %q is not running", name)
	}
	child.Interrupt()
	return map[string]any{"target": name, "previousStatus": "running"}, nil
}

// RegisterTeamSession declares one lead session's Team runtime on demand and
// returns a teardown that stops live children and unregisters it. The agent
// loop (or any host owning a lead session) calls this once per lead session;
// without it the fold-based tools still work, but spawn_teammate has no child
// registry and wait_agent/interrupt/send have no live peer handles. Safe to
// call repeatedly for the same session id.
func RegisterTeamSession(sessionID string) func() {
	globalTeam.registerLead(sessionID)
	return func() { globalTeam.unregisterLead(sessionID) }
}

// RegisterTeamTools installs the model-facing Agent Teams tool set (upstream
// @deepseek-ai/tool-agent-team): spawn_teammate, send_message, followup_task,
// list_agents, wait_agent, interrupt_agent, team_task_create, team_task_list,
// team_task_get, and team_task_update. Team tools are opt-in; wiring (Phase 2)
// decides whether to call this from the shared pipeline.
func (r *ToolRegistry) RegisterTeamTools() {
	r.Register(ToolDefinition{
		Name:        "spawn_teammate",
		Description: "Create one named, durable teammate. Only the Team Lead may call this tool.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": { "type": "string", "description": "Unique lower-kebab-case teammate name." },
				"description": { "type": "string", "description": "Short description of the delegated responsibility." },
				"prompt": { "type": "string", "description": "Complete initial task for the teammate." },
				"context": { "type": "string", "enum": ["fresh", "fork"], "description": "fresh starts without Lead history; fork inherits completed Lead turns. Defaults to fresh." }
			},
			"required": ["name", "description", "prompt"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Prompt      string `json:"prompt"`
				Context     string `json:"context"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if args.Context == "" {
				args.Context = "fresh"
			}
			member, err := globalTeam.spawnTeammate(ctx, args.Name, args.Description, args.Prompt, args.Context)
			if err != nil {
				return nil, err
			}
			return map[string]any{"member": member}, nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "send_message",
		Description: "Send durable information to another Team member without starting an idle member.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"target": { "type": "string", "description": "Team member name, or lead." },
				"message": { "type": "string", "description": "Self-contained message for the target." }
			},
			"required": ["target", "message"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Target  string `json:"target"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			return globalTeam.teamMessageSend(ctx, args.Target, args.Message, "quiet")
		},
	})

	r.Register(ToolDefinition{
		Name:        "followup_task",
		Description: "Send a durable follow-up task to another Team member and start a turn when needed.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"target": { "type": "string", "description": "Team member name, or lead." },
				"message": { "type": "string", "description": "Self-contained message for the target." }
			},
			"required": ["target", "message"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Target  string `json:"target"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			return globalTeam.teamMessageSend(ctx, args.Target, args.Message, "wakeup")
		},
	})

	r.Register(ToolDefinition{
		Name:           "list_agents",
		Description:    "List the Lead and every durable teammate with current runtime status.",
		ParametersJSON: json.RawMessage(`{"type": "object", "properties": {}, "required": []}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			return globalTeam.teamListMembers(ctx)
		},
	})

	r.Register(ToolDefinition{
		Name:        "wait_agent",
		Description: "Wait for the next teammate status, mailbox, or shared-task change after this call starts. This never wakes inactive members and returns noProgress immediately when no other member is running or provisioning. Re-list after wakeup or timeout instead of polling.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"timeout_ms": { "type": "integer", "description": "Wait duration in milliseconds, from 10000 through 3600000. Defaults to 30000." }
			},
			"required": []
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				TimeoutMs int `json:"timeout_ms"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if args.TimeoutMs == 0 {
				args.TimeoutMs = 30000
			}
			return globalTeam.teamWait(ctx, args.TimeoutMs)
		},
	})

	r.Register(ToolDefinition{
		Name:        "interrupt_agent",
		Description: "Interrupt one teammate's current turn while preserving its pending inbox. Team Lead only.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"target": { "type": "string", "description": "Teammate name." }
			},
			"required": ["target"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Target string `json:"target"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			return globalTeam.interruptAgent(ctx, args.Target)
		},
	})

	r.Register(ToolDefinition{
		Name:        "team_task_create",
		Description: "Create one unowned pending task on the shared Team task board.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"subject": { "type": "string", "description": "Concise task title." },
				"description": { "type": "string", "description": "Complete task details and acceptance criteria." },
				"blocked_by": { "type": "array", "items": { "type": "string" }, "description": "Task ids that must complete first." },
				"write_scopes": { "type": "array", "items": { "type": "string" }, "description": "Advisory workspace-relative file or directory prefixes this task expects to modify." }
			},
			"required": ["subject", "description"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Subject     string   `json:"subject"`
				Description string   `json:"description"`
				BlockedBy   []string `json:"blocked_by"`
				WriteScopes []string `json:"write_scopes"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			return globalTeam.teamTaskCreate(ctx, args.Subject, args.Description, args.BlockedBy, args.WriteScopes)
		},
	})

	r.Register(ToolDefinition{
		Name:        "team_task_list",
		Description: "List shared tasks, including readiness, owner, revision, blockers, and write-scope warnings.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"status": { "type": "string", "enum": ["pending", "in_progress", "completed"], "description": "Optional exact status filter." }
			},
			"required": []
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			tasks, err := globalTeam.teamTaskList(ctx, args.Status)
			if err != nil {
				return nil, err
			}
			return map[string]any{"tasks": tasks}, nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "team_task_get",
		Description: "Read the complete latest value of one shared task before changing or executing it.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"task_id": { "type": "string", "description": "Shared task id." }
			},
			"required": ["task_id"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			return globalTeam.teamTaskGet(ctx, args.TaskID)
		},
	})

	r.Register(ToolDefinition{
		Name:        "team_task_update",
		Description: "Compare-and-set a shared task action using the latest revision from team_task_get or team_task_list.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"task_id": { "type": "string", "description": "Shared task id." },
				"expected_revision": { "type": "integer", "description": "Current task revision used as the CAS precondition." },
				"action": { "type": "string", "enum": ["claim", "release", "edit", "set_dependencies", "complete", "reopen", "reassign", "delete"], "description": "Task transition to apply." },
				"subject": { "type": "string", "description": "Replacement title for edit." },
				"description": { "type": "string", "description": "Replacement details for edit." },
				"blocked_by": { "type": "array", "items": { "type": "string" }, "description": "Complete blocker list for set_dependencies." },
				"write_scopes": { "type": "array", "items": { "type": "string" }, "description": "Replacement advisory write scopes for edit." },
				"owner": { "type": "string", "description": "Member name for reassign; omit to unassign." }
			},
			"required": ["task_id", "expected_revision", "action"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				TaskID           string   `json:"task_id"`
				ExpectedRevision int      `json:"expected_revision"`
				Action           string   `json:"action"`
				Subject          *string  `json:"subject"`
				Description      *string  `json:"description"`
				BlockedBy        []string `json:"blocked_by"`
				WriteScopes      []string `json:"write_scopes"`
				Owner            *string  `json:"owner"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			fields := map[string]any{}
			if args.Subject != nil {
				fields["subject"] = *args.Subject
			}
			if args.Description != nil {
				fields["description"] = *args.Description
			}
			if args.BlockedBy != nil {
				fields["blocked_by"] = toAnySlice(args.BlockedBy)
			}
			if args.WriteScopes != nil {
				fields["write_scopes"] = toAnySlice(args.WriteScopes)
			}
			if args.Owner != nil {
				fields["owner"] = *args.Owner
			}
			return globalTeam.teamTaskUpdate(ctx, args.TaskID, args.ExpectedRevision, args.Action, fields)
		},
	})
}
