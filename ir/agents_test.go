package ir

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestAgentRoleUnmarshal confirms a top-level agents: map decodes into stable
// AgentRole values, with the convenience fields (model/system_prompt) and the
// opaque with: preserved.
func TestAgentRoleUnmarshal(t *testing.T) {
	const src = `{
		"workflow": "w",
		"version": 1,
		"containers": {},
		"agents": {
			"auditor": {
				"uses": "anthropic/claude-code",
				"model": "opus",
				"system_prompt": "audit carefully",
				"output_schema": {"type": "object"},
				"with": {"mcp_servers": ["memclaw"]}
			},
			"writer": {
				"uses": "openai/codex"
			}
		},
		"graph": []
	}`
	var wf Workflow
	if err := json.Unmarshal([]byte(src), &wf); err != nil {
		t.Fatal(err)
	}
	if len(wf.Agents) != 2 {
		t.Fatalf("got %d roles, want 2", len(wf.Agents))
	}
	aud := wf.Agents["auditor"]
	if aud.Uses != "anthropic/claude-code" {
		t.Errorf("auditor.Uses = %q", aud.Uses)
	}
	if aud.Model != "opus" {
		t.Errorf("auditor.Model = %q", aud.Model)
	}
	if aud.SystemPrompt != "audit carefully" {
		t.Errorf("auditor.SystemPrompt = %q", aud.SystemPrompt)
	}
	if aud.OutputSchema == nil || (*aud.OutputSchema)["type"] != "object" {
		t.Errorf("auditor.OutputSchema = %v", aud.OutputSchema)
	}
	servers, ok := aud.With["mcp_servers"].([]any)
	if !ok || len(servers) != 1 || servers[0] != "memclaw" {
		t.Errorf("auditor.With[mcp_servers] = %v", aud.With["mcp_servers"])
	}
	writer := wf.Agents["writer"]
	if writer.Uses != "openai/codex" {
		t.Errorf("writer.Uses = %q", writer.Uses)
	}
	if writer.Model != "" || writer.SystemPrompt != "" || writer.OutputSchema != nil || writer.With != nil {
		t.Errorf("writer should have empty optionals: %+v", writer)
	}
}

// TestRoleByName covers the hit/miss accessor shared by the validator and the
// run-start resolver.
func TestRoleByName(t *testing.T) {
	wf := &Workflow{Agents: map[string]AgentRole{
		"auditor": {Uses: "anthropic/claude-code"},
	}}
	if r, ok := wf.RoleByName("auditor"); !ok || r.Uses != "anthropic/claude-code" {
		t.Fatalf("RoleByName(auditor) = %+v, %v", r, ok)
	}
	if _, ok := wf.RoleByName("ghost"); ok {
		t.Fatalf("RoleByName(ghost) should miss")
	}
	// A workflow with no agents: must miss cleanly (nil map read).
	empty := &Workflow{}
	if _, ok := empty.RoleByName("auditor"); ok {
		t.Fatalf("RoleByName on nil Agents should miss")
	}
}

// TestAgentRoleRoundTrip confirms marshal→unmarshal preserves the role shape
// (so the digest and resume pinning see a stable form).
func TestAgentRoleRoundTrip(t *testing.T) {
	wf := &Workflow{
		ID:         "w",
		Version:    1,
		Containers: map[string]Container{},
		Agents: map[string]AgentRole{
			"auditor": {
				Uses:         "anthropic/claude-code",
				Model:        "opus",
				SystemPrompt: "audit",
				OutputSchema: &JSONSchema{"type": "object"},
				With:         RawConfig{"mcp_servers": []any{"memclaw"}},
			},
		},
		Graph: NodeList{},
	}
	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	var got Workflow
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wf.Agents, got.Agents) {
		t.Fatalf("round-trip changed Agents:\n got  = %+v\n want = %+v", got.Agents, wf.Agents)
	}
}
