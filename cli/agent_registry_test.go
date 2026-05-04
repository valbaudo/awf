package cli

import (
	"os"
	"testing"

	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/container"
)

func TestBuildAgentRegistry_EmptyAllowlist_NoAdapter(t *testing.T) {
	be := container.NewFake()
	reg, err := buildAgentRegistry(nil, be)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	if _, ok := reg.Lookup(claude.AdapterRef); ok {
		t.Error("Lookup found Claude adapter; want absent (empty allowlist)")
	}
}

func TestBuildAgentRegistry_WithAllowlist_RegistersClaude(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-fixture")
	be := container.NewFake()
	reg, err := buildAgentRegistry([]string{"ANTHROPIC_API_KEY"}, be)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	a, ok := reg.Lookup(claude.AdapterRef)
	if !ok {
		t.Fatal("Lookup missed Claude adapter")
	}
	if a.Ref() != claude.AdapterRef {
		t.Errorf("Ref = %q", a.Ref())
	}
}

func TestBuildAgentRegistry_AllowlistedKeyAbsentFromHost_StillRegisters(t *testing.T) {
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	be := container.NewFake()
	reg, err := buildAgentRegistry([]string{"ANTHROPIC_API_KEY"}, be)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	if _, ok := reg.Lookup(claude.AdapterRef); !ok {
		t.Error("Lookup missed Claude adapter; want present (build doesn't fail on missing host env)")
	}
}

func TestBuildAgentRegistry_NilBackend_Errors(t *testing.T) {
	_, err := buildAgentRegistry([]string{"ANTHROPIC_API_KEY"}, nil)
	if err == nil {
		t.Fatal("err nil; want error when Backend is nil (adapter needs it for Version + Launch)")
	}
}
