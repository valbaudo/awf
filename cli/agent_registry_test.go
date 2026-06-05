package cli

import (
	"os"
	"testing"

	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/agent/droid"
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
	if err := os.Unsetenv("ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("Unsetenv ANTHROPIC_API_KEY: %v", err)
	}
	if err := os.Unsetenv("ANTHROPIC_AUTH_TOKEN"); err != nil {
		t.Fatalf("Unsetenv ANTHROPIC_AUTH_TOKEN: %v", err)
	}
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

func TestBuildAgentRegistry_RegistersDroid(t *testing.T) {
	reg, err := buildAgentRegistry([]string{"ANTHROPIC_API_KEY", "FACTORY_API_KEY"}, container.NewFake())
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	if _, ok := reg.Lookup(droid.AdapterRef); !ok {
		t.Errorf("registry has no adapter for %q", droid.AdapterRef)
	}
	if _, ok := reg.Lookup("anthropic/claude-code"); !ok {
		t.Errorf("registry lost the claude adapter")
	}
}

func TestBuildAgentRegistry_RegistersGoose(t *testing.T) {
	reg, err := buildAgentRegistry([]string{"GOOSE_PROVIDER"}, container.NewFake())
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	if _, ok := reg.Lookup("block/goose"); !ok {
		t.Errorf("block/goose not registered")
	}
}

func TestDefaultAgentEnv_NoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range defaultAgentEnv {
		if seen[k] {
			t.Errorf("defaultAgentEnv contains duplicate %q", k)
		}
		seen[k] = true
	}
	for _, want := range []string{"GOOSE_PROVIDER", "GOOSE_MODEL"} {
		if !seen[want] {
			t.Errorf("defaultAgentEnv missing %q", want)
		}
	}
}

func TestDefaultAgentEnv_IncludesBothVendors(t *testing.T) {
	has := func(name string) bool {
		for _, n := range defaultAgentEnv {
			if n == name {
				return true
			}
		}
		return false
	}
	if !has("ANTHROPIC_API_KEY") || !has("FACTORY_API_KEY") {
		t.Errorf("defaultAgentEnv = %v, want both ANTHROPIC_API_KEY and FACTORY_API_KEY", defaultAgentEnv)
	}
}

func TestBuildAgentRegistry_RegistersCodex(t *testing.T) {
	reg, err := buildAgentRegistry([]string{"OPENAI_API_KEY"}, container.NewFake())
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	if _, ok := reg.Lookup("openai/codex"); !ok {
		t.Errorf("openai/codex not registered")
	}
	if _, ok := reg.Lookup("anthropic/claude-code"); !ok {
		t.Errorf("registry lost the claude adapter")
	}
}

func TestDefaultAgentEnv_IncludesCodexVars(t *testing.T) {
	has := func(name string) bool {
		for _, n := range defaultAgentEnv {
			if n == name {
				return true
			}
		}
		return false
	}
	if !has("OPENAI_API_KEY") || !has("CODEX_HOME") {
		t.Errorf("defaultAgentEnv = %v, want OPENAI_API_KEY and CODEX_HOME", defaultAgentEnv)
	}
	// dedup: OPENAI_API_KEY overlaps goose — must appear exactly once.
	seen, n := map[string]bool{}, 0
	for _, k := range defaultAgentEnv {
		if k == "OPENAI_API_KEY" {
			n++
		}
		if seen[k] {
			t.Errorf("defaultAgentEnv has duplicate %q", k)
		}
		seen[k] = true
	}
	if n != 1 {
		t.Errorf("OPENAI_API_KEY appears %d times, want 1", n)
	}
}

func TestBuildAgentRegistry_RegistersAWFLLM(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	reg, err := buildAgentRegistry([]string{"OPENAI_API_KEY"}, container.NewFake())
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	if _, ok := reg.Lookup("awf/llm"); !ok {
		t.Error("awf/llm not registered")
	}
}

func TestDefaultAgentEnv_NoDuplicateOpenAIKey(t *testing.T) {
	seen := 0
	for _, k := range defaultAgentEnv {
		if k == "OPENAI_API_KEY" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("OPENAI_API_KEY appears %d times in defaultAgentEnv, want 1 (dedup)", seen)
	}
}
