package ir

import "strings"

// validateAgents checks the top-level agents: map and every uses: ref.
//
//   - Each role must declare a non-empty uses: base adapter ref (AWF1033 —
//     the role *definition* fault).
//   - A role NAME must NOT contain '/': the <vendor>/<name> form is reserved for
//     base adapter refs, so a role named "anthropic/claude-code" would be
//     ambiguous with a real adapter (AWF1033 — role-vs-adapter name collision).
//   - Every step's uses: must resolve to EITHER a declared role OR a
//     syntactically-valid base adapter ref (contains '/'); anything else is
//     AWF1034 (the *reference* fault). The validator cannot know which base refs
//     are *registered* (registration is CLI-time, cli/agent_registry.go); the
//     run-start resolver issues the hard *ErrAdapterNotFound on an unknown
//     adapter. This pass only proves the static grammar.
func validateAgents(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	wf := ld.Workflow
	for name, role := range wf.Agents {
		rolePath := "agents." + name
		if strings.TrimSpace(role.Uses) == "" {
			c.errf(rolePath, "AWF1033", "agents: role "+name+" has empty uses: (must name an existing adapter)")
		}
		if strings.Contains(name, "/") {
			c.errf(rolePath, "AWF1033", "agents: role name "+name+" must not contain '/' (the <vendor>/<name> form is reserved for adapter refs)")
		}
	}
	WalkNodes(wf.Graph, "", func(n Node, nodePath string) {
		as, ok := n.(*AgentStep)
		if !ok {
			return
		}
		if _, isRole := wf.RoleByName(as.Uses); isRole {
			return
		}
		// Not a role → must be a base adapter ref (the <vendor>/<name> grammar).
		if !strings.Contains(as.Uses, "/") {
			c.errf(nodePath, "AWF1034", "uses: "+as.Uses+" is neither a declared agents: role nor a <vendor>/<name> adapter ref")
		}
	})
}
