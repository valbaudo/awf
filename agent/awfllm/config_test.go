package awfllm_test

import (
	"testing"

	"github.com/valbaudo/awf/agent"
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
