package easyapi

import (
	"context"

	"dsh-go/core"
)

// SafeMessage appends a user message through the real Core safe context.
func SafeMessage(ctx core.SafeContext, id core.ContextID, text string) error {
	if ctx == nil {
		return errNilContext
	}
	return ctx.AppendUserMessage(context.Background(), id, text)
}

// SafeChunk appends an assistant chunk through the real Core safe context.
func SafeChunk(ctx core.SafeContext, id core.ContextID, chunk string) error {
	if ctx == nil {
		return errNilContext
	}
	return ctx.AppendAssistantChunk(context.Background(), id, chunk)
}

// UnsafeRewrite requires explicit unsafe permission through the real Core gate.
func UnsafeRewrite(ux core.UnsafeContext, id core.ContextID, patch string) error {
	if ux == nil {
		return errNilContext
	}
	return ux.RewriteHistory(context.Background(), id, patch)
}
