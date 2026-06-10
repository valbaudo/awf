package ir

import "testing"

// Tests for the agents: role-definition + uses:-resolution pass — see validate_agents.go
// (AWF1033 = role definition fault; AWF1034 = uses: reference fault).

func TestValidateAgentsEmptyUses(t *testing.T) {
	// A role with an empty uses: is a malformed definition → AWF1033.
	ld := makeLD(&Workflow{
		ID: "empty-uses", Version: 1,
		Agents:     map[string]AgentRole{"auditor": {Uses: ""}},
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF1033", "agents.auditor")
}

func TestValidateAgentsRoleUsesMustBeBaseAdapterRef(t *testing.T) {
	tests := []struct {
		name  string
		roles map[string]AgentRole
		path  string
	}{
		{
			name: "same module role name",
			roles: map[string]AgentRole{
				"writer":   {Uses: "reviewer"},
				"reviewer": {Uses: "anthropic/claude-code"},
			},
			path: "agents.writer",
		},
		{
			name: "bare non adapter name",
			roles: map[string]AgentRole{
				"writer": {Uses: "claude"},
			},
			path: "agents.writer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ld := makeLD(&Workflow{
				ID: "role-base-ref", Version: 1,
				Agents:     tt.roles,
				Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
				Graph:      NodeList{},
			})
			assertErrorAt(t, Validate(ld), "AWF1033", tt.path)
		})
	}
}

func TestValidateAgentsRoleNameWithSlash(t *testing.T) {
	// A role name that contains '/' collides with the <vendor>/<name> adapter-ref
	// form → AWF1033 (role-vs-adapter name collision).
	ld := makeLD(&Workflow{
		ID: "slash-name", Version: 1,
		Agents:     map[string]AgentRole{"anthropic/claude-code": {Uses: "anthropic/claude-code"}},
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph:      NodeList{},
	})
	assertErrorAt(t, Validate(ld), "AWF1033", "agents.anthropic/claude-code")
}

func TestValidateAgentsUsesUndeclaredNonRef(t *testing.T) {
	// uses: that is neither a declared role nor a <vendor>/<name> form → AWF1034.
	ld := makeLD(&Workflow{
		ID: "bad-uses", Version: 1,
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{ID: "triage", Container: "lab", Uses: "undeclared-role"},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1034", "triage")
}

func TestValidateAgentsUsesDeclaredRole(t *testing.T) {
	// uses: names a declared role → no AWF1034.
	ld := makeLD(&Workflow{
		ID: "role-ok", Version: 1,
		Agents:     map[string]AgentRole{"auditor": {Uses: "anthropic/claude-code"}},
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{ID: "triage", Container: "lab", Uses: "auditor"},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF1034")
}

func TestValidateAgentsUsesBaseRefNoRole(t *testing.T) {
	// uses: a <vendor>/<name> base ref with no declared role → no AWF1034.
	ld := makeLD(&Workflow{
		ID: "base-ref-ok", Version: 1,
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{ID: "triage", Container: "lab", Uses: "anthropic/claude-code"},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF1034")
}
