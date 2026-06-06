package cli

import (
	"errors"
	"os"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/agent/droid"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

func TestMergeWorkflowEnv(t *testing.T) {
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	cases := []struct {
		name     string
		base     []string
		workflow []string
		want     []string
	}{
		{"no workflow env returns base unchanged", []string{"A", "B"}, nil, []string{"A", "B"}},
		{"workflow extends base, order-stable", []string{"A", "B"}, []string{"C"}, []string{"A", "B", "C"}},
		{"overlap deduped, first occurrence wins", []string{"A", "B"}, []string{"B", "C"}, []string{"A", "B", "C"}},
		{"empty base + workflow names forwards the workflow set", nil, []string{"LITELLM_API_KEY"}, []string{"LITELLM_API_KEY"}},
		{"empty workflow names are skipped", []string{"A"}, []string{""}, []string{"A"}},
		{"both empty stays nil", nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeWorkflowEnv(tc.base, tc.workflow)
			if !eq(got, tc.want) {
				t.Errorf("mergeWorkflowEnv(%v, %v) = %v, want %v", tc.base, tc.workflow, got, tc.want)
			}
		})
	}
}

func TestMergeWorkflowEnv_DoesNotAliasBase(t *testing.T) {
	// The returned slice must be fresh: mutating it must not corrupt the caller's
	// base allowlist (the empty-workflow path used to return base by reference).
	base := []string{"A", "B"}
	got := mergeWorkflowEnv(base, nil)
	if len(got) > 0 {
		got[0] = "MUTATED"
	}
	if base[0] != "A" {
		t.Errorf("mergeWorkflowEnv aliased base: mutating the result changed base to %v", base)
	}
}

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

func TestRegisterRoles_RegistersDerivedAdapter(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	reg, err := buildAgentRegistry([]string{"ANTHROPIC_API_KEY"}, container.NewFake())
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	wf := &ir.Workflow{
		Agents: map[string]ir.AgentRole{
			"auditor": {
				Uses:  claude.AdapterRef,
				Model: "opus",
				With:  ir.RawConfig{"mcp_servers": []any{"m"}},
			},
		},
	}
	if err := registerRoles(reg, wf); err != nil {
		t.Fatalf("registerRoles: %v", err)
	}
	a, ok := reg.Lookup("auditor")
	if !ok {
		t.Fatal("Lookup missed the auditor role")
	}
	if _, isDerived := a.(*agent.DerivedAdapter); !isDerived {
		t.Errorf("auditor adapter type = %T, want *agent.DerivedAdapter", a)
	}
	if a.Ref() != "auditor" {
		t.Errorf("Ref = %q, want %q", a.Ref(), "auditor")
	}
}

func TestRegisterRoles_NilOrEmptyAgents_NoOp(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	reg, err := buildAgentRegistry([]string{"ANTHROPIC_API_KEY"}, container.NewFake())
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	if err := registerRoles(reg, nil); err != nil {
		t.Errorf("registerRoles(nil) = %v, want nil", err)
	}
	if err := registerRoles(reg, &ir.Workflow{}); err != nil {
		t.Errorf("registerRoles(empty agents) = %v, want nil", err)
	}
}

func TestRegisterRoles_UnregisteredBase_ErrAdapterNotFound(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	reg, err := buildAgentRegistry([]string{"ANTHROPIC_API_KEY"}, container.NewFake())
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	wf := &ir.Workflow{
		Agents: map[string]ir.AgentRole{
			"ghost": {Uses: "vendor/never-registered"},
		},
	}
	err = registerRoles(reg, wf)
	var notFound *agent.ErrAdapterNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("registerRoles error = %v, want *agent.ErrAdapterNotFound", err)
	}
	if notFound.Ref != "vendor/never-registered" {
		t.Errorf("ErrAdapterNotFound.Ref = %q, want %q", notFound.Ref, "vendor/never-registered")
	}
}

func TestRegisterRoles_NameCollidesWithRegisteredRef_ErrAlreadyRegistered(t *testing.T) {
	// AWF1033 already rejects '/' in role names statically, but a defensive
	// run-start collision (a role whose name equals a registered ref) must still
	// surface *ErrAdapterAlreadyRegistered from Registry.Register.
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	reg, err := buildAgentRegistry([]string{"ANTHROPIC_API_KEY"}, container.NewFake())
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	wf := &ir.Workflow{
		Agents: map[string]ir.AgentRole{
			claude.AdapterRef: {Uses: claude.AdapterRef},
		},
	}
	err = registerRoles(reg, wf)
	var dup *agent.ErrAdapterAlreadyRegistered
	if !errors.As(err, &dup) {
		t.Fatalf("registerRoles error = %v, want *agent.ErrAdapterAlreadyRegistered", err)
	}
}

func TestRoleWithFor_FoldsConvenienceKeys(t *testing.T) {
	schema := ir.JSONSchema{"type": "object"}
	role := ir.AgentRole{
		Uses:         claude.AdapterRef,
		Model:        "opus",
		SystemPrompt: "audit",
		OutputSchema: &schema,
		With:         ir.RawConfig{"mcp_servers": []any{"m"}},
	}
	got := roleWithFor(role)
	if got["model"] != "opus" {
		t.Errorf("model = %v, want opus", got["model"])
	}
	if got["system_prompt"] != "audit" {
		t.Errorf("system_prompt = %v, want audit", got["system_prompt"])
	}
	if got["output_schema"] == nil {
		t.Errorf("output_schema absent, want folded")
	}
	if got["mcp_servers"] == nil {
		t.Errorf("mcp_servers dropped, want preserved from role with:")
	}
}

func TestRoleWithFor_ExplicitWithKeyWins(t *testing.T) {
	// An explicit with:.model must NOT be clobbered by the convenience Model field.
	role := ir.AgentRole{
		Uses:  claude.AdapterRef,
		Model: "opus",
		With:  ir.RawConfig{"model": "sonnet"},
	}
	got := roleWithFor(role)
	if got["model"] != "sonnet" {
		t.Errorf("model = %v, want sonnet (explicit with: wins over convenience field)", got["model"])
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
