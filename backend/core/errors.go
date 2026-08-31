package core

import "errors"

var (
	errEmptyText         = errors.New("core: context text is empty")
	errMissingOwner      = errors.New("core: context owner is required")
	errMissingTool       = errors.New("core: tool name is required")
	errMissingAttachment = errors.New("core: attachment name and path are required")
	errUnsafeForbidden   = errors.New("core: unsafe context permission denied")
	errNilPlugin         = errors.New("core: nil plugin")
	errEmptyPluginID     = errors.New("core: plugin id is required")
	errHooksUnavailable  = errors.New("core/hooks: event bus unavailable")

	ErrAlreadyLoaded = errors.New("core: plugin already loaded")
	ErrUnknownPlugin = errors.New("core: unknown plugin")
	ErrBuildFailed   = errors.New("core: reload build failed")
	ErrCloseFailed   = errors.New("core: owner close failed")
	ErrNilTask       = errors.New("core: worker task fn is required")
	ErrNilSession    = errors.New("core: worker task requires a session id")
	ErrPoolClosed    = errors.New("core: worker pool is closed")
	ErrTaskPending   = errors.New("core: worker task still pending")
)
