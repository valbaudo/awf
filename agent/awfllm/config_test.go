package awfllm_test

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/ir"
)

func TestBuildReqConfig_GeminiDefaults(t *testing.T) {
	a := llmAdapter(t, map[string]string{"GEMINI_API_KEY": "k"})
	cfg, err := a.BuildReqConfigForTest(agent.AgentInvocation{
		With: ir.RawConfig{"provider": "gemini", "model": "gemini-3.5-flash"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "gemini" {
		t.Fatalf("provider=%q", cfg.Provider)
	}
	if cfg.BaseURL != "https://generativelanguage.googleapis.com" {
		t.Fatalf("default gemini base_url=%q", cfg.BaseURL)
	}
	if cfg.APIKey != "k" { // resolved from GEMINI_API_KEY by default for gemini
		t.Fatalf("apiKey=%q", cfg.APIKey)
	}
}

func TestValidateConfig_ProviderEnum(t *testing.T) {
	a := llmAdapter(t, okEnv())
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "model": "m", "provider": "bogus"}); err == nil {
		t.Fatal("expected rejection of unknown provider")
	}
}

// TestBuildReqConfig_OpenAIDefaults: provider omitted → openai defaults unchanged
// (existing base_url + OPENAI_API_KEY), and MaxInlineBytes is the 32 MiB default.
func TestBuildReqConfig_OpenAIDefaults(t *testing.T) {
	a := llmAdapter(t, okEnv()) // OPENAI_API_KEY=sk-test
	cfg, err := a.BuildReqConfigForTest(agent.AgentInvocation{
		With: ir.RawConfig{"model": "gpt-x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openai" {
		t.Fatalf("provider=%q, want openai (default)", cfg.Provider)
	}
	if cfg.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("default openai base_url=%q", cfg.BaseURL)
	}
	if cfg.APIKey != "sk-test" {
		t.Fatalf("apiKey=%q, want sk-test (OPENAI_API_KEY)", cfg.APIKey)
	}
	if cfg.MaxInlineBytes != 32<<20 {
		t.Fatalf("MaxInlineBytes=%d, want %d (32 MiB default)", cfg.MaxInlineBytes, 32<<20)
	}
}

// TestBuildReqConfig_OllamaKeyOptional — Fix B: the ollama transport is a LOCAL
// server, so its API key env is optional. With provider:ollama and NO key env in
// the allowlist, buildReqConfig succeeds and leaves cfg.APIKey == "" (streamOllama
// omits the Authorization header on an empty key). Base URL defaults to localhost.
func TestBuildReqConfig_OllamaKeyOptional(t *testing.T) {
	a := llmAdapter(t, map[string]string{}) // no key env at all
	cfg, err := a.BuildReqConfigForTest(agent.AgentInvocation{
		With: ir.RawConfig{"provider": "ollama", "model": "llama3"},
	})
	if err != nil {
		t.Fatalf("buildReqConfig (ollama, no key) must succeed: %v", err)
	}
	if cfg.Provider != "ollama" {
		t.Fatalf("provider=%q, want ollama", cfg.Provider)
	}
	if cfg.APIKey != "" {
		t.Fatalf("apiKey=%q, want empty (ollama key optional)", cfg.APIKey)
	}
	if cfg.BaseURL != "http://localhost:11434" {
		t.Fatalf("default ollama base_url=%q, want http://localhost:11434", cfg.BaseURL)
	}
}

// TestEffectiveProviderAndDefaults_NoDrift — Fix A/B: the single-source helpers.
// effectiveProvider returns ollama for a bare structured_output:ollama_format (the
// back-compat path with no provider key); and providerDefaults returns the gemini
// key env that buildReqConfig actually resolves (no drift between validate + build).
func TestEffectiveProviderAndDefaults_NoDrift(t *testing.T) {
	// effectiveProvider: bare ollama_format → ollama (back-compat).
	if got := awfllm.EffectiveProviderForTest(ir.RawConfig{"structured_output": "ollama_format"}); got != "ollama" {
		t.Errorf("effectiveProvider(ollama_format) = %q, want ollama", got)
	}
	// effectiveProvider: explicit provider wins.
	if got := awfllm.EffectiveProviderForTest(ir.RawConfig{"provider": "gemini"}); got != "gemini" {
		t.Errorf("effectiveProvider(provider:gemini) = %q, want gemini", got)
	}
	// effectiveProvider: nothing set → openai default.
	if got := awfllm.EffectiveProviderForTest(ir.RawConfig{"model": "m"}); got != "openai" {
		t.Errorf("effectiveProvider(bare) = %q, want openai", got)
	}
	// providerDefaults(gemini) names GEMINI_API_KEY — the same env buildReqConfig resolves.
	_, geminiKeyEnv := awfllm.ProviderDefaultsForTest("gemini")
	if geminiKeyEnv != "GEMINI_API_KEY" {
		t.Fatalf("providerDefaults(gemini) key env = %q, want GEMINI_API_KEY", geminiKeyEnv)
	}
	a := llmAdapter(t, map[string]string{geminiKeyEnv: "k"})
	with := ir.RawConfig{"provider": "gemini", "model": "m", "prompt": "hi"}
	if err := a.ValidateConfig(with); err != nil {
		t.Fatalf("ValidateConfig(provider:gemini) with %s present should pass: %v", geminiKeyEnv, err)
	}
	cfg, err := a.BuildReqConfigForTest(agent.AgentInvocation{With: with})
	if err != nil {
		t.Fatalf("buildReqConfig(provider:gemini): %v", err)
	}
	if cfg.APIKey != "k" {
		t.Fatalf("buildReqConfig resolved apiKey=%q, want k (from %s) — drift between validate+build", cfg.APIKey, geminiKeyEnv)
	}
}

func TestBuildReqConfig_AnthropicDefaults(t *testing.T) {
	a, _ := awfllm.New(awfllm.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-ant"}))
	cfg, err := a.BuildReqConfigForTest(agent.AgentInvocation{With: ir.RawConfig{
		"provider": "anthropic", "model": "claude-sonnet-4-6", "prompt": "hi",
		"cache_system": true, "cache_documents": true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", cfg.Provider)
	}
	if cfg.BaseURL != "https://api.anthropic.com" {
		t.Errorf("BaseURL = %q, want anthropic host", cfg.BaseURL)
	}
	if cfg.APIKey != "sk-ant" {
		t.Errorf("APIKey = %q, want sk-ant (resolved ANTHROPIC_API_KEY)", cfg.APIKey)
	}
	if !cfg.CacheSystem || !cfg.CacheDocuments {
		t.Errorf("cache flags not set: %+v", cfg)
	}
}

func TestBuildReqConfig_GeminiCacheParsed(t *testing.T) {
	a := llmAdapter(t, map[string]string{"GEMINI_API_KEY": "k"})
	cfg, err := a.BuildReqConfigForTest(agent.AgentInvocation{With: ir.RawConfig{
		"provider": "gemini", "model": "gemini-2.5-pro", "prompt": "x",
		"gemini_cache": map[string]any{"mode": "explicit", "ttl": "120s"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GeminiCache == nil || cfg.GeminiCache.Mode != "explicit" || cfg.GeminiCache.TTL != "120s" {
		t.Fatalf("GeminiCache parse wrong: %+v", cfg.GeminiCache)
	}
	off, err := a.BuildReqConfigForTest(agent.AgentInvocation{With: ir.RawConfig{"provider": "gemini", "model": "m", "prompt": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if off.GeminiCache != nil {
		t.Errorf("absent gemini_cache must yield nil, got %+v", off.GeminiCache)
	}
}

// TestBuildReqConfig_MaxInlineBytesOverride: max_inline_bytes with-key overrides
// the default.
func TestBuildReqConfig_MaxInlineBytesOverride(t *testing.T) {
	a := llmAdapter(t, okEnv())
	cfg, err := a.BuildReqConfigForTest(agent.AgentInvocation{
		With: ir.RawConfig{"model": "gpt-x", "max_inline_bytes": 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxInlineBytes != 1024 {
		t.Fatalf("MaxInlineBytes=%d, want 1024 (override)", cfg.MaxInlineBytes)
	}
}
