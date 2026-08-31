package plugin

import (
	"fmt"

	"dsh-go/pkg/tools"
)

// webCapability is the first real builtin capability. It moves the existing
// production web tools behind the plugin boundary while retaining the host's
// live ToolRegistry, CommandRegistry, EventBus and permission policy.
type webCapability struct{}

func (webCapability) Name() string { return "web" }

func (webCapability) Mount(cap *Capabilities) (Disposer, error) {
	if cap == nil || cap.Tools == nil {
		return nil, fmt.Errorf("plugin(web): tool registry unavailable")
	}
	before := cap.Tools.Names()
	cap.Tools.RegisterWebTools()
	beforeSet := make(map[string]bool, len(before))
	for _, name := range before {
		beforeSet[name] = true
	}
	owned := make([]string, 0, 2)
	for _, name := range cap.Tools.Names() {
		if !beforeSet[name] || name == "web_search" || name == "web_fetch" {
			owned = append(owned, name)
		}
	}
	cap.Tools.ClaimOwner("web", owned...)
	return func() {
		for _, name := range owned {
			cap.Tools.UnregisterOwned("web", name)
		}
	}, nil
}

var _ Capability = webCapability{}
var _ = tools.DefaultApprovalPolicy
