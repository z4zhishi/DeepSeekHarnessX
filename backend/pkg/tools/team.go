package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"dsh-go/pkg/session"
)

// Agent Teams durable fold (upstream packages/experimental/agent-team/src/fold.ts):
// the four log-only Team event types are owned by the Team Lead session and
// replay into members, tasks, and mailbox state.

const teamEventVersion = 1

var (
	memberNameRe      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	taskIDRe          = regexp.MustCompile(`^task-(\d+)$`)
	teamMemberNameMax = 64
)

// teamFoldState is the pure replay result for one Team.
type teamFoldState struct {
	id          string
	members     map[string]session.TeamMemberRecord
	memberNames map[string]string // name -> member id
	tasks       map[string]session.TeamTaskRecord
	messages    map[string]session.TeamMessageRecord
	delivered   map[string]bool
	nextTaskID  int
}

func emptyTeamFoldState() *teamFoldState {
	return &teamFoldState{
		members:     map[string]session.TeamMemberRecord{},
		memberNames: map[string]string{},
		tasks:       map[string]session.TeamTaskRecord{},
		messages:    map[string]session.TeamMessageRecord{},
		delivered:   map[string]bool{},
		nextTaskID:  1,
	}
}

// foldTeamEvents replays one session log's Team events, skipping records that
// belong to a different Team id (fork inheritance).
func foldTeamEvents(events []*session.SessionEnvelope, teamID string) (*teamFoldState, error) {
	state := emptyTeamFoldState()
	state.id = teamID
	for _, env := range events {
		switch env.Type {
		case session.EventTeamMember:
			var p session.TeamMemberPayload
			if err := json.Unmarshal(env.Data, &p); err != nil {
				return nil, fmt.Errorf("persisted team/member payload is invalid")
			}
			if p.Version != teamEventVersion || p.TeamID != teamID {
				continue
			}
			if err := applyTeamMember(state, p.Member); err != nil {
				return nil, err
			}
		case session.EventTeamTask:
			var p session.TeamTaskPayload
			if err := json.Unmarshal(env.Data, &p); err != nil {
				return nil, fmt.Errorf("persisted team/task payload is invalid")
			}
			if p.Version != teamEventVersion || p.TeamID != teamID {
				continue
			}
			if err := applyTeamTask(state, p.Task); err != nil {
				return nil, err
			}
		case session.EventTeamMessageQueued:
			var p session.TeamMessagePayload
			if err := json.Unmarshal(env.Data, &p); err != nil {
				return nil, fmt.Errorf("persisted team/message/queued payload is invalid")
			}
			if p.Version != teamEventVersion || p.TeamID != teamID {
				continue
			}
			state.messages[p.Message.ID] = p.Message
		case session.EventTeamMessageDelivered:
			var p session.TeamMessageDeliveredPayload
			if err := json.Unmarshal(env.Data, &p); err != nil {
				return nil, fmt.Errorf("persisted team/message/delivered payload is invalid")
			}
			if p.Version != teamEventVersion || p.TeamID != teamID {
				continue
			}
			state.delivered[p.MessageID] = true
		}
	}
	return state, nil
}

// applyTeamMember enforces roster invariants: first entry must be
// provisioning; identity fields never change; names are never reused.
func applyTeamMember(state *teamFoldState, m session.TeamMemberRecord) error {
	prior, exists := state.members[m.ID]
	if named, ok := state.memberNames[m.Name]; ok && named != m.ID {
		return fmt.Errorf("teammate name %q is reused by another member", m.Name)
	}
	if !exists {
		if m.Phase != "provisioning" {
			return fmt.Errorf("teammate %q must begin provisioning", m.Name)
		}
		state.memberNames[m.Name] = m.ID
	} else {
		if prior.Name != m.Name || prior.Provider != m.Provider || prior.Context != m.Context {
			return fmt.Errorf("teammate %q changed immutable identity fields", m.ID)
		}
		if prior.Phase != "provisioning" || m.Phase == "provisioning" {
			return fmt.Errorf("teammate %q has an invalid %s -> %s transition", m.Name, prior.Phase, m.Phase)
		}
	}
	state.members[m.ID] = m
	return nil
}

// applyTeamTask enforces contiguous revisions, numeric id allocation, and
// graph validity (no self-block, no cycles, existing blockers).
func applyTeamTask(state *teamFoldState, t session.TeamTaskRecord) error {
	prior, exists := state.tasks[t.ID]
	if !exists && t.Revision != 1 {
		return fmt.Errorf("team task %q must begin at revision 1", t.ID)
	}
	if exists && t.Revision != prior.Revision+1 {
		return fmt.Errorf("team task %q revision is not contiguous", t.ID)
	}
	if err := validateTaskGraph(state.tasks, t); err != nil {
		return err
	}
	if m := taskIDRe.FindStringSubmatch(t.ID); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n >= state.nextTaskID {
			state.nextTaskID = n + 1
		}
	}
	state.tasks[t.ID] = t
	return nil
}

// validateTaskGraph rejects self-dependency, unknown blockers, and cycles.
func validateTaskGraph(tasks map[string]session.TeamTaskRecord, candidate session.TeamTaskRecord) error {
	seen := map[string]bool{}
	for _, b := range candidate.BlockedBy {
		if b == candidate.ID {
			return fmt.Errorf("a team task cannot block itself")
		}
		if seen[b] {
			return fmt.Errorf("duplicate blocker %q", b)
		}
		blocker, ok := tasks[b]
		if !ok || blocker.Status == "deleted" {
			return fmt.Errorf("blocker task %q not found", b)
		}
		seen[b] = true
	}
	// DFS cycle check over the whole graph (candidate included once stored).
	all := make(map[string]session.TeamTaskRecord, len(tasks)+1)
	for id, t := range tasks {
		all[id] = t
	}
	all[candidate.ID] = candidate
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(id string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, dep := range all[id].BlockedBy {
			if visit(dep) {
				return true
			}
		}
		visiting[id] = false
		visited[id] = true
		return false
	}
	for id := range all {
		if visit(id) {
			return fmt.Errorf("team task dependency cycle")
		}
	}
	return nil
}

// teamMemberView is the runtime-enriched roster row.
type teamMemberView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Role        string   `json:"role"` // "lead" | "teammate"
	Status      string   `json:"status"`
	Description string   `json:"description,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Context     string   `json:"context,omitempty"`
	Model       string   `json:"model,omitempty"`
	Diagnostics []string `json:"diagnostics"`
}

// teamTaskView is the runtime-enriched task row.
type teamTaskView struct {
	ID                 string   `json:"id"`
	Revision           int      `json:"revision"`
	Subject            string   `json:"subject"`
	Description        string   `json:"description"`
	Status             string   `json:"status"`
	OwnerName          string   `json:"ownerName,omitempty"`
	BlockedBy          []string `json:"blockedBy"`
	WriteScopes        []string `json:"writeScopes"`
	Ready              bool     `json:"ready"`
	WriteScopeWarnings []string `json:"writeScopeWarnings"`
}

// scopesOverlap reports prefix overlap on path components (advisory only).
func scopesOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

// teamTaskViewOf builds the runtime view from a durable record.
func teamTaskViewOf(state *teamFoldState, t session.TeamTaskRecord) teamTaskView {
	owner := ""
	if t.OwnerID != "" {
		if t.OwnerID == state.id {
			owner = "lead"
		} else if m, ok := state.members[t.OwnerID]; ok {
			owner = m.Name
		}
	}
	ready := true
	for _, b := range t.BlockedBy {
		if state.tasks[b].Status != "completed" {
			ready = false
			break
		}
	}
	var warnings []string
	for _, other := range state.tasks {
		if other.ID == t.ID || other.Status != "in_progress" {
			continue
		}
		overlap := false
		for _, a := range t.WriteScopes {
			for _, b := range other.WriteScopes {
				if scopesOverlap(a, b) {
					overlap = true
					break
				}
			}
			if overlap {
				break
			}
		}
		if overlap {
			warnings = append(warnings, "overlaps "+other.ID)
		}
	}
	sort.Strings(warnings)
	return teamTaskView{
		ID:                 t.ID,
		Revision:           t.Revision,
		Subject:            t.Subject,
		Description:        t.Description,
		Status:             t.Status,
		OwnerName:          owner,
		BlockedBy:          t.BlockedBy,
		WriteScopes:        t.WriteScopes,
		Ready:              ready,
		WriteScopeWarnings: warnings,
	}
}
