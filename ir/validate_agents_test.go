package ir

import (
	"strings"
	"testing"
)

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

// --- AWF1067: role model/system_prompt/top-level-string with: templates are
// input.* only; any template in a nested/non-string position is rejected outright. ---

// rolesGuard1067 validates a workflow whose only agents: entry is `role` (referenced
// by one step so the workflow is otherwise well-formed) and returns the AWF1067
// diagnostic messages found (empty if none).
func rolesGuard1067(t *testing.T, role AgentRole) []string {
	t.Helper()
	ld := makeLD(&Workflow{
		ID: "role-template", Version: 1,
		Agents:     map[string]AgentRole{"r": role},
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{ID: "s", Container: "c", Uses: "r", With: RawConfig{"prompt": "hi"}},
		},
	})
	var msgs []string
	for _, d := range Validate(ld) {
		if d.Code == "AWF1067" {
			msgs = append(msgs, d.Message)
		}
	}
	return msgs
}

func TestRoleTemplate_InputRoot_OK(t *testing.T) {
	got := rolesGuard1067(t, AgentRole{Uses: "openai/codex", Model: "{{ input.model }}"})
	if len(got) != 0 {
		t.Fatalf("expected no AWF1067 for input.* root, got %v", got)
	}
}

func TestRoleTemplate_NonInputRoot_Rejected(t *testing.T) {
	for _, ref := range []string{"{{ run.id }}", "{{ step.x.y }}", "{{ item.z }}"} {
		got := rolesGuard1067(t, AgentRole{Uses: "openai/codex", Model: ref})
		if len(got) == 0 {
			t.Fatalf("expected AWF1067 for non-input root %q, got none", ref)
		}
	}
}

func TestRoleTemplate_SystemPromptAndStringWith_Checked(t *testing.T) {
	got := rolesGuard1067(t, AgentRole{
		Uses:         "openai/codex",
		SystemPrompt: "{{ run.id }}",                        // non-input → reject
		With:         RawConfig{"api_base": "{{ step.a }}"}, // top-level string, non-input → reject
	})
	if len(got) < 2 {
		t.Fatalf("expected AWF1067 for both system_prompt and with.api_base, got %v", got)
	}
}

func TestRoleTemplate_NestedTemplate_Rejected(t *testing.T) {
	// A template in a NESTED position is never substituted (would leak literally).
	got := rolesGuard1067(t, AgentRole{
		Uses: "openai/codex",
		With: RawConfig{"mcp_servers": []any{map[string]any{"url": "{{ input.url }}"}}},
	})
	if len(got) == 0 {
		t.Fatalf("expected AWF1067 for nested template, got none")
	}
	if !strings.Contains(strings.ToLower(got[0]), "nested") {
		t.Fatalf("message should name the nested position, got %q", got[0])
	}
}

func TestRoleTemplate_MalformedTemplate_Rejected(t *testing.T) {
	got := rolesGuard1067(t, AgentRole{Uses: "openai/codex", Model: "{{ input.model"}) // unterminated
	if len(got) == 0 {
		t.Fatalf("expected AWF1067 for malformed template, got none")
	}
}
