package tools

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"dsh-go/pkg/session"
)

// DefaultSandboxMode is the deployment default beneath a session override.
// workspace-write keeps the existing agent experience (files under the
// session workspace are writable) while still confining writes; read-only
// and danger-full-access are the explicit fail-safe / full-access poles.
const DefaultSandboxMode = session.SandboxWorkspaceWrite

// DefaultApprovalPolicy is the deployment default beneath a session override:
// ask (interactive answerers decide; missing answerers fail closed).
const DefaultApprovalPolicy = session.ApprovalPolicyAsk

// SessionPolicy is one session's resolved policy knobs (upstream
// sandbox-policy session-mode + user-approval effective folds).
type SessionPolicy struct {
	Sandbox  session.SandboxMode
	Approval session.ApprovalPolicy
	Preset   string
}

// PolicyStore holds per-session policy state; the session log events are the
// durable source, this map is the live mirror updated on each switch.
type PolicyStore struct {
	mu      sync.RWMutex
	byID    map[string]SessionPolicy
	watches map[string]func(SessionPolicy)
}

// NewPolicyStore builds an empty store; sessions resolve defaults on first
// read (no entry needed).
func NewPolicyStore() *PolicyStore {
	return &PolicyStore{
		byID:    map[string]SessionPolicy{},
		watches: map[string]func(SessionPolicy){},
	}
}

// Get resolves the session's policy, applying deployment defaults beneath.
func (p *PolicyStore) Get(sessionID string) SessionPolicy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pol, ok := p.byID[sessionID]
	if !ok {
		return SessionPolicy{Sandbox: DefaultSandboxMode, Approval: DefaultApprovalPolicy}
	}
	return pol
}

// SetSandboxMode records one sandbox/mode switch: validates the mode, updates
// the in-memory state, and reports the durable event through emit (the agent
// loop or RPC layer supplies it; nil disables emission for direct tests).
func (p *PolicyStore) SetSandboxMode(sessionID string, mode session.SandboxMode, source string, emit func(eventType string, payload any)) error {
	if mode != session.SandboxReadOnly && mode != session.SandboxWorkspaceWrite && mode != session.SandboxDangerFullAccess {
		return fmt.Errorf("invalid sandbox mode %q", mode)
	}
	p.mu.Lock()
	pol := p.byID[sessionID]
	pol.Sandbox = mode
	p.byID[sessionID] = pol
	p.mu.Unlock()
	if emit != nil {
		payload := session.SandboxModePayload{Mode: mode}
		if source != "" {
			payload.Source = source
		}
		emit(session.EventSandboxMode, payload)
	}
	return nil
}

// SetApprovalPolicy records one approval/policy override (ask | never).
func (p *PolicyStore) SetApprovalPolicy(sessionID string, policy session.ApprovalPolicy, source string, emit func(eventType string, payload any)) error {
	if policy != session.ApprovalPolicyAsk && policy != session.ApprovalPolicyNever {
		return fmt.Errorf("invalid approval policy %q", policy)
	}
	p.mu.Lock()
	pol := p.byID[sessionID]
	pol.Approval = policy
	p.byID[sessionID] = pol
	p.mu.Unlock()
	if emit != nil {
		payload := session.ApprovalPolicyPayload{Policy: policy}
		if source != "" {
			payload.Source = source
		}
		emit(session.EventApprovalPolicy, payload)
	}
	return nil
}

// SetPreset records one permission/preset selection (durable user intent;
// the knob events follow in the same turn).
func (p *PolicyStore) SetPreset(sessionID, preset string, emit func(eventType string, payload any)) error {
	if preset == "" {
		return fmt.Errorf("preset must be a non-empty string")
	}
	p.mu.Lock()
	pol := p.byID[sessionID]
	pol.Preset = preset
	p.byID[sessionID] = pol
	p.mu.Unlock()
	if emit != nil {
		emit(session.EventPermissionPreset, session.PermissionPresetPayload{Preset: preset})
	}
	return nil
}

// checkWrite resolves the sandbox boundary for one file-writing tool call.
// Returns the denial marker text (upstream sandbox denial vocabulary) when
// the mode forbids the write; empty means allowed.
func (p *PolicyStore) checkWrite(sessionID, cwd, targetPath string) string {
	pol := p.Get(sessionID)
	switch pol.Sandbox {
	case session.SandboxDangerFullAccess:
		return ""
	case session.SandboxReadOnly:
		return "[sandbox: file access denied under read-only mode]"
	default: // workspace-write
		if cwd == "" {
			// No workspace boundary available: the write is not provably
			// inside any workspace, so it is denied under the workspace mode.
			return "[sandbox: file access denied under workspace-write mode]"
		}
		resolved := targetPath
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(cwd, resolved)
		}
		rel, err := filepath.Rel(cwd, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "[sandbox: file access denied under workspace-write mode]"
		}
		return ""
	}
}
