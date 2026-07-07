package ir

import (
	"fmt"
	"sort"
	"strings"

	"github.com/valbaudo/awf/template"
)

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
		uses := strings.TrimSpace(role.Uses)
		if uses == "" {
			c.errf(rolePath, "AWF1033", "agents: role "+name+" has empty uses: (must name an existing adapter)")
		} else if !strings.Contains(uses, "/") {
			c.errf(rolePath, "AWF1033", "agents: role "+name+" uses "+uses+"; role uses: must name a <vendor>/<name> adapter ref")
		}
		if strings.Contains(name, "/") {
			c.errf(rolePath, "AWF1033", "agents: role name "+name+" must not contain '/' (the <vendor>/<name> form is reserved for adapter refs)")
		}
		checkRoleTemplates(rolePath, role, c)
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

// checkRoleTemplates enforces AWF1067: a role's model/system_prompt and each
// top-level string with: value may reference {{ input.* }} ONLY; any template in
// a nested/non-string position (never substituted at run time) is rejected
// outright so it cannot leak to the adapter as literal text. Mirrors the
// top-level-string substitution surface of engine.substituteRawConfig.
func checkRoleTemplates(rolePath string, role AgentRole, c *collector) {
	checkRoleInputOnly(rolePath+".model", role.Model, c)
	checkRoleInputOnly(rolePath+".system_prompt", role.SystemPrompt, c)
	// Top-level with: string values → input.* allowed; everything nested → no templates.
	keys := make([]string, 0, len(role.With))
	for k := range role.With {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := role.With[k].(string); ok {
			checkRoleInputOnly(rolePath+".with."+k, s, c)
			continue
		}
		rejectAnyRoleTemplate(rolePath+".with."+k, role.With[k], c)
	}
}

// checkRoleInputOnly: every {{ }} slot in src must parse and have root "input".
func checkRoleInputOnly(path, src string, c *collector) {
	if src == "" {
		return
	}
	slots, err := template.Slots(src)
	if err != nil {
		c.errf(path, "AWF1067", "role config has a malformed template: "+src)
		return
	}
	for _, sl := range slots {
		inner := strings.TrimSpace(sl.Inner)
		ref, perr := template.ParseRef(inner)
		if perr != nil || len(ref.Segments) == 0 {
			c.errf(path, "AWF1067", "role config has an invalid reference {{ "+inner+" }}")
			continue
		}
		if ref.Segments[0].Ident != "input" {
			c.errf(path, "AWF1067", "role config may reference {{ input.* }} only; found {{ "+inner+" }}")
		}
	}
}

// rejectAnyRoleTemplate: recurse into nested values; ANY {{ }} in a string leaf
// here is rejected — nested positions are not substituted at run time.
func rejectAnyRoleTemplate(path string, v any, c *collector) {
	switch t := v.(type) {
	case string:
		if slots, err := template.Slots(t); err != nil || len(slots) > 0 {
			c.errf(path, "AWF1067", "role config has a template in a nested position (never substituted); allowed only in model, system_prompt, and top-level string with: values")
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			rejectAnyRoleTemplate(path+"."+k, t[k], c)
		}
	case []any:
		for i, e := range t {
			rejectAnyRoleTemplate(fmt.Sprintf("%s[%d]", path, i), e, c)
		}
	}
}
