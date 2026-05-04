package claude

import (
	"testing"
)

func TestNew_DefaultConstruct(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a == nil {
		t.Fatal("New returned nil adapter")
	}
}

func TestAdapter_Ref(t *testing.T) {
	a, _ := New()
	if a.Ref() != AdapterRef {
		t.Errorf("Ref() = %q, want %q", a.Ref(), AdapterRef)
	}
}

func TestAdapter_Capabilities_NativeSchemaTrue(t *testing.T) {
	a, _ := New()
	caps := a.Capabilities()
	if !caps.NativeSchema {
		t.Error("Capabilities().NativeSchema = false; want true (Claude Code's --json-schema)")
	}
}

func TestWithEnv_PopulatesAllowlist(t *testing.T) {
	a, err := New(WithEnv(map[string]string{
		"ANTHROPIC_API_KEY": "sk-test",
		"OTHER":             "v",
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	keys := a.envAllowlist()
	if _, ok := keys["ANTHROPIC_API_KEY"]; !ok {
		t.Error("ANTHROPIC_API_KEY missing from allowlist")
	}
	if _, ok := keys["OTHER"]; !ok {
		t.Error("OTHER missing from allowlist")
	}
}

func TestWithEnv_EmptyMapOK(t *testing.T) {
	a, err := New(WithEnv(map[string]string{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(a.envAllowlist()) != 0 {
		t.Errorf("envAllowlist len = %d; want 0", len(a.envAllowlist()))
	}
}

// var _ agent.Adapter = (*Adapter)(nil) // uncommented after Task 16 (Launch defined)
