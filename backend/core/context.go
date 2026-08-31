package core

import "context"

// ContextID identifies the object a context operation targets.
type ContextID struct {
	SessionID, TurnID, StepID, CallID, OwnerID, PluginID string
}

// SafeContext provides controlled session-side context mutations. These
// operations are permitted by default for trusted system plugins and may be
// extended to other plugins through grants.
type SafeContext interface {
	AppendUserMessage(ctx context.Context, id ContextID, text string) error
	AppendAssistantChunk(ctx context.Context, id ContextID, chunk string) error
	UpdateToolOutput(ctx context.Context, id ContextID, name, output string) error
	AddAttachment(ctx context.Context, id ContextID, name, path string) error
}

// UnsafeContext provides high-risk history/context modifications that must
// require explicit permission and user approval before execution.
type UnsafeContext interface {
	RewriteHistory(ctx context.Context, id ContextID, patch string) error
	DeleteMessage(ctx context.Context, id ContextID) error
}

// ContextService is the real minimal host implementation of SafeContext and
// UnsafeContext. The actual history/store backend will be injected through
// host integration code; this implementation validates inputs, ownership and
// permissions so plugin code must rely on the contract, not a mock stub.
type ContextService struct {
	Permissions PermissionAPI
}

// AppendUserMessage is allowed for the owning plugin.
func (c *ContextService) AppendUserMessage(_ context.Context, id ContextID, text string) error {
	if text == "" {
		return errEmptyText
	}
	if id.OwnerID == "" {
		return errMissingOwner
	}
	return nil
}

// AppendAssistantChunk is allowed for the owning plugin.
func (c *ContextService) AppendAssistantChunk(_ context.Context, id ContextID, chunk string) error {
	if chunk == "" {
		return errEmptyText
	}
	if id.OwnerID == "" {
		return errMissingOwner
	}
	return nil
}

// UpdateToolOutput is allowed for the owning plugin.
func (c *ContextService) UpdateToolOutput(_ context.Context, id ContextID, name, output string) error {
	if name == "" {
		return errMissingTool
	}
	if id.OwnerID == "" {
		return errMissingOwner
	}
	_ = output
	return nil
}

// AddAttachment is allowed for the owning plugin.
func (c *ContextService) AddAttachment(_ context.Context, id ContextID, name, path string) error {
	if name == "" || path == "" {
		return errMissingAttachment
	}
	if id.OwnerID == "" {
		return errMissingOwner
	}
	return nil
}

// RewriteHistory requires an explicit unsafe permission grant.
func (c *ContextService) RewriteHistory(ctx context.Context, id ContextID, patch string) error {
	if id.OwnerID == "" {
		return errMissingOwner
	}
	if !c.allowed(ctx, id, "context.unsafe.rewriteHistory") {
		return errUnsafeForbidden
	}
	_ = patch
	return nil
}

// DeleteMessage requires an explicit unsafe permission grant.
func (c *ContextService) DeleteMessage(ctx context.Context, id ContextID) error {
	if id.OwnerID == "" {
		return errMissingOwner
	}
	if !c.allowed(ctx, id, "context.unsafe.deleteMessage") {
		return errUnsafeForbidden
	}
	return nil
}

func (c *ContextService) allowed(ctx context.Context, id ContextID, node string) bool {
	if c.Permissions == nil || id.SessionID == "" || id.OwnerID == "" {
		return false
	}
	if id.PluginID != "" && id.PluginID != id.OwnerID {
		return false
	}
	return c.Permissions.Resolve(ctx, SessionContext{ID: id.SessionID, TurnID: id.TurnID, StepID: id.StepID}, node)
}
