package tools

import (
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

func callerID(ctx ToolExecutionContext) string { return ctx.CallerID }

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
