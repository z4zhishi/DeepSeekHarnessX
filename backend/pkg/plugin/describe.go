package plugin

import (
	"sort"
	"strings"

	"dsh-go/pkg/tools"
)

// RegistryView is the read-only description exported by a Registry snapshot.
// It contains only state observed from the registry, tool/command tables, and
// event bus; it does not probe or invent plugin capabilities.
type RegistryView struct {
	Metadata         []PluginInfo      `json:"metadata"`
	Capabilities     []CapabilityView  `json:"capabilities"`
	Events           []EventView       `json:"events"`
	Permissions      []PermissionView  `json:"permissions"`
	ToolOwnership    map[string]string `json:"toolOwnership"`
	CommandOwnership map[string]string `json:"commandOwnership"`
}

// CapabilityView is one runtime tool or slash-command capability observed from
// the live registry. Plugin-owned tools keep their "<plugin>__" prefix so the
// caller can trace ownership back to a Metadata entry.
type CapabilityView struct {
	Plugin       string `json:"plugin,omitempty"`
	Name         string `json:"name"`
	Kind         string `json:"kind"` // tool | command
	Description  string `json:"description,omitempty"`
	InputSchema  string `json:"inputSchema,omitempty"`
	RequiresPerm bool   `json:"requiresPerm"`
	Owner        string `json:"owner,omitempty"`
}

// EventView describes one event topic and its current subscriber count.
type EventView struct {
	Topic       string `json:"topic"`
	Subscribers int    `json:"subscribers"`
}

// PermissionView reports the permission gate attached to one registered tool.
// Approval and sandbox are deployment defaults; per-session overrides remain
// in tools.PolicyStore and are intentionally not fabricated here.
type PermissionView struct {
	Tool            string `json:"tool"`
	RequiresPerm    bool   `json:"requiresPerm"`
	DefaultApproval string `json:"defaultApproval"`
	DefaultSandbox  string `json:"defaultSandbox"`
}

// Describe exports a deterministic snapshot of the registry's live state.
//
// The snapshot reflects only live registrations in the tool/command/event
// registries. It does not run the plugin handshake, start external hosts, or
// infer capabilities not already present in the process. Per-session overrides
// live in tools.PolicyStore and are intentionally absent from the view.
//
// The returned value is a plain data snapshot; callers may marshal it without
// retaining references to mutable registry internals.
func (r *Registry) Describe() RegistryView {
	if r == nil {
		return RegistryView{}
	}
	view := RegistryView{Metadata: r.ListInfo()}
	if r.tools != nil {
		view.ToolOwnership = r.tools.ToolOwners()
		if r.tools.Commands != nil {
			view.CommandOwnership = r.tools.Commands.OwnerMap()
		}
		for _, def := range r.tools.ListDeclarations() {
			view.Capabilities = append(view.Capabilities, CapabilityView{
				Plugin:       capabilityOwner(def.Name),
				Name:         def.Name,
				Kind:         "tool",
				Description:  def.Description,
				InputSchema:  string(def.ParametersJSON),
				RequiresPerm: def.RequiresPerm,
				Owner:        def.Owner,
			})
			view.Permissions = append(view.Permissions, PermissionView{
				Tool:            def.Name,
				RequiresPerm:    def.RequiresPerm,
				DefaultApproval: string(tools.DefaultApprovalPolicy),
				DefaultSandbox:  string(tools.DefaultSandboxMode),
			})
		}
		if r.tools.Commands != nil {
			for _, def := range r.tools.Commands.List() {
				view.Capabilities = append(view.Capabilities, CapabilityView{
					Name:        def.Name,
					Kind:        "command",
					Description: def.Description,
					Owner:       def.Owner,
				})
			}
		}
	}
	if r.bus != nil {
		view.Events = r.bus.Describe()
	}
	sort.Slice(view.Capabilities, func(i, j int) bool {
		if view.Capabilities[i].Name == view.Capabilities[j].Name {
			return view.Capabilities[i].Kind < view.Capabilities[j].Kind
		}
		return view.Capabilities[i].Name < view.Capabilities[j].Name
	})
	sort.Slice(view.Permissions, func(i, j int) bool { return view.Permissions[i].Tool < view.Permissions[j].Tool })
	return view
}

func capabilityOwner(name string) string {
	if i := strings.Index(name, "__"); i > 0 {
		return name[:i]
	}
	return ""
}
