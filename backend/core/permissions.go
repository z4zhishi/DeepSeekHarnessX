package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// PermissionAPI is the public contract for permission checks.
type PermissionAPI interface {
	RegisterNode(NodeDescriptor) error
	Resolve(ctx context.Context, session SessionContext, node string) bool
	Require(ctx context.Context, session SessionContext, node string) error
	Grant(target Target, grant Grant) error
	Revoke(target Target, grantID string) error
	Describe(node string) (NodeDescriptor, bool)
	ExpireGrant(target Target, grantID string)
	Tick()
}

// NodeDescriptor is the public description of a permission node.
type NodeDescriptor struct {
	ID, Description, Group, Parent, DangerLevel, Scope string
	Default, RequiresApproval                          bool
}

// Target identifies the subject receiving a grant.
type Target struct {
	Kind, ID string
}

// Grant records an explicit permission override.
type Grant struct {
	ID, PluginID, Node, Reason string
	Duration                   time.Duration
	ExpiresAt                  time.Time
	CreatedAt                  time.Time
}

// PermissionService is a complete, real implementation of PermissionAPI.
type PermissionService struct {
	mu     sync.Mutex
	nodes  map[string]NodeDescriptor
	grants map[string]map[string]Grant // targetKey -> grantID -> Grant
}

// NewPermissionService creates an empty service.
func NewPermissionService() *PermissionService {
	return &PermissionService{nodes: map[string]NodeDescriptor{}, grants: map[string]map[string]Grant{}}
}

// RegisterNode adds or replaces a permission node.
func (ps *PermissionService) RegisterNode(nd NodeDescriptor) error {
	if nd.ID == "" {
		return errors.New("core: permission node id is required")
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.nodes[nd.ID] = nd
	return nil
}

func targetKey(target Target) string {
	return fmt.Sprintf("%s/%s", target.Kind, target.ID)
}

// Resolve checks the permission for the given session.
func (ps *PermissionService) Resolve(_ context.Context, session SessionContext, node string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	nd, ok := ps.nodes[node]
	if !ok {
		return false
	}
	if nd.Default {
		return true
	}
	if session.ID == "" {
		return false
	}
	key := targetKey(Target{Kind: "session", ID: session.ID})
	if grants, ok := ps.grants[key]; ok {
		now := time.Now()
		for _, g := range grants {
			if g.Node != node {
				continue
			}
			if g.ExpiresAt.IsZero() || g.ExpiresAt.After(now) {
				return true
			}
		}
	}
	return false
}

// Grant adds a permission override.
func (ps *PermissionService) Grant(target Target, grant Grant) error {
	if target.Kind == "" || target.ID == "" || grant.ID == "" || grant.Node == "" {
		return errors.New("core: grant requires target and grant fields")
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	key := targetKey(target)
	if ps.grants[key] == nil {
		ps.grants[key] = map[string]Grant{}
	}
	if grant.CreatedAt.IsZero() {
		grant.CreatedAt = time.Now()
	}
	if grant.Duration > 0 && grant.ExpiresAt.IsZero() {
		grant.ExpiresAt = grant.CreatedAt.Add(grant.Duration)
	}
	ps.grants[key][grant.ID] = grant
	return nil
}

// Revoke removes a grant.
func (ps *PermissionService) Revoke(target Target, grantID string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	key := targetKey(target)
	if _, ok := ps.grants[key]; !ok {
		return fmt.Errorf("core: unknown target for revoke %s", key)
	}
	if _, ok := ps.grants[key][grantID]; !ok {
		return fmt.Errorf("core: unknown grant %s", grantID)
	}
	delete(ps.grants[key], grantID)
	if len(ps.grants[key]) == 0 {
		delete(ps.grants, key)
	}
	return nil
}

// Describe returns the metadata for a node.
func (ps *PermissionService) Describe(node string) (NodeDescriptor, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	nd, ok := ps.nodes[node]
	return nd, ok
}

// ExpireGrant immediately removes a grant, effectively revoking it without
// returning an error when the grant or target is already absent.
func (ps *PermissionService) ExpireGrant(target Target, grantID string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	key := targetKey(target)
	if _, ok := ps.grants[key]; !ok {
		return
	}
	delete(ps.grants[key], grantID)
	if len(ps.grants[key]) == 0 {
		delete(ps.grants, key)
	}
}

// Require resolves a permission and returns a standardized error when access is
// denied.
func (ps *PermissionService) Require(ctx context.Context, session SessionContext, node string) error {
	if ps.Resolve(ctx, session, node) {
		return nil
	}
	return fmt.Errorf("core: permission %s denied", node)
}

func (ps *PermissionService) Tick() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := time.Now()
	for target, grants := range ps.grants {
		for id, g := range grants {
			if g.ExpiresAt.IsZero() {
				continue
			}
			if g.ExpiresAt.Before(now) {
				delete(grants, id)
			}
		}
		if len(grants) == 0 {
			delete(ps.grants, target)
		}
	}
}
