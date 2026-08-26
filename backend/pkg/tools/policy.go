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
	Sandbox     session.SandboxMode
	Approval    session.ApprovalPolicy
	Preset      string
	ReviewModel string // small-model reviewer id used by ApprovalPolicyReview (auto mode)
}

// Mode returns the high-level permission preset derived from the resolved
// knobs (used by the session.policy RPC and the GUI read-back so the frontend
// dropdown always reflects the real state, not a stale default).
func (p SessionPolicy) Mode() session.PermissionMode {
	if p.Approval == session.ApprovalPolicyNever && p.Sandbox == session.SandboxReadOnly {
		return session.PermissionModePlan
	}
	if p.Approval == session.ApprovalPolicyAllowAll {
		return session.PermissionModeAllowAll
	}
	if p.Approval == session.ApprovalPolicyReview {
		return session.PermissionModeAutoReview
	}
	if p.Approval == session.ApprovalPolicyAcceptEdits {
		return session.PermissionModeAcceptEdits
	}
	if p.Approval == session.ApprovalPolicyAsk && p.Sandbox == session.SandboxDangerFullAccess {
		// Legacy unrestricted wrote full-access but kept ask; still surface as
		// allow-all intent so the GUI reflects "everything allowed on disk".
		return session.PermissionModeAllowAll
	}
	return session.PermissionModeDefault
}

// PolicyStore holds per-session policy state; the session log events are the
// durable source, this map is the live mirror updated on each switch.
type PolicyStore struct {
	mu      sync.RWMutex
	byID    map[string]SessionPolicy
	watches map[string]func(SessionPolicy)
	// allowedTools 是「本会话总是允许」的会话级工具白名单（allow_all
	// 审批决策的记忆面）。仅存内存：审批记忆的存活期与会话驻留一致，
	// 进程重启后重新询问是更安全的缺省。
	allowedTools map[string]map[string]bool
}

// NewPolicyStore builds an empty store; sessions resolve defaults on first
// read (no entry needed).
func NewPolicyStore() *PolicyStore {
	return &PolicyStore{
		byID:         map[string]SessionPolicy{},
		watches:      map[string]func(SessionPolicy){},
		allowedTools: map[string]map[string]bool{},
	}
}

// PolicyAllowed 报告该会话是否已通过 allow_all 记忆放行某工具。
func (p *PolicyStore) PolicyAllowed(sessionID, toolName string) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.allowedTools[sessionID][toolName]
}

// RememberApproval 把一个工具记入会话级 allow_all 白名单。
func (p *PolicyStore) RememberApproval(sessionID, toolName string) {
	if p == nil || toolName == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.allowedTools[sessionID] == nil {
		p.allowedTools[sessionID] = map[string]bool{}
	}
	p.allowedTools[sessionID][toolName] = true
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

// SetApprovalPolicy records one approval/policy override (ask | never |
// accept-edits | review | allow-all).
func (p *PolicyStore) SetApprovalPolicy(sessionID string, policy session.ApprovalPolicy, source string, emit func(eventType string, payload any)) error {
	switch policy {
	case session.ApprovalPolicyAsk, session.ApprovalPolicyNever,
		session.ApprovalPolicyAcceptEdits, session.ApprovalPolicyReview,
		session.ApprovalPolicyAllowAll:
	default:
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

// SetReviewModel records the small-model reviewer id used by the review
// (auto) approval policy. Validated against the ApprovalPolicyReview mode.
func (p *PolicyStore) SetReviewModel(sessionID, model string, emit func(eventType string, payload any)) error {
	if model == "" {
		return fmt.Errorf("review model must be a non-empty model id")
	}
	p.mu.Lock()
	pol := p.byID[sessionID]
	pol.ReviewModel = model
	p.byID[sessionID] = pol
	p.mu.Unlock()
	if emit != nil {
		emit(session.EventPermissionPreset, session.PermissionPresetPayload{
			Preset:      string(pol.Preset),
			ReviewModel: model,
		})
	}
	return nil
}

// SetMode applies a high-level permission preset as the (sandbox, approval)
// pair in one call, then emits both knob events so a resumed session replays
// the same policy. Auto/plan/default resolve reviewModel to empty here; the
// GUI sets ReviewModel separately via SetReviewModel when it wants a reviewer.
func (p *PolicyStore) SetMode(sessionID string, mode session.PermissionMode, emit func(eventType string, payload any)) error {
	var sandbox session.SandboxMode
	var approval session.ApprovalPolicy
	switch mode {
	case session.PermissionModeDefault:
		sandbox, approval = session.SandboxWorkspaceWrite, session.ApprovalPolicyAsk
	case session.PermissionModeAcceptEdits:
		sandbox, approval = session.SandboxWorkspaceWrite, session.ApprovalPolicyAcceptEdits
	case session.PermissionModePlan:
		sandbox, approval = session.SandboxReadOnly, session.ApprovalPolicyNever
	case session.PermissionModeAutoReview:
		sandbox, approval = session.SandboxWorkspaceWrite, session.ApprovalPolicyReview
	case session.PermissionModeAllowAll:
		sandbox, approval = session.SandboxDangerFullAccess, session.ApprovalPolicyAllowAll
	default:
		return fmt.Errorf("invalid permission mode %q", mode)
	}
	if err := p.SetSandboxMode(sessionID, sandbox, "", emit); err != nil {
		return err
	}
	if err := p.SetApprovalPolicy(sessionID, approval, "", emit); err != nil {
		return err
	}
	return p.SetPreset(sessionID, string(mode), emit)
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
