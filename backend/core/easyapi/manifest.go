package easyapi

import "dsh-go/core"

// Manifest describes the optional EasyAPI developer library/plugin.
var Manifest = core.Metadata{
	ID:          "dshx-easy-api",
	Version:     "0.1.0",
	APIVersion:  "1",
	Description: "Optional convenience API for context, events and session tasks",
	Capabilities: []core.Capability{
		{ID: "easyapi.context.safe", Type: "api", Description: "Controlled session context helpers"},
		{ID: "easyapi.context.unsafe", Type: "api", Description: "High-risk context helpers"},
		{ID: "easyapi.events", Type: "api", Description: "Scoped event helpers"},
		{ID: "easyapi.tasks", Type: "api", Description: "Session task helpers"},
	},
}
