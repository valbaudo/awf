package awfllm_test

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/ir"
)

func llmAdapter(t *testing.T, env map[string]string) *awfllm.Adapter {
	t.Helper()
	a, err := awfllm.New(awfllm.WithEnv(env))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func okEnv() map[string]string { return map[string]string{"OPENAI_API_KEY": "sk-test"} }

func TestValidate_RequiresModelAndPrompt(t *testing.T) {
	a := llmAdapter(t, okEnv())
	var bad *agent.ErrInvalidConfig
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "hi"}); !errors.As(err, &bad) || bad.Key != "model" {
		t.Fatalf("missing model = %v, want ErrInvalidConfig{model}", err)
	}
	if err := a.ValidateConfig(ir.RawConfig{"model": "m"}); !errors.As(err, &bad) || bad.Key != "prompt" {
		t.Fatalf("missing prompt = %v, want ErrInvalidConfig{prompt}", err)
	}
}

func TestValidate_UnknownKey(t *testing.T) {
	a := llmAdapter(t, okEnv())
	var bad *agent.ErrInvalidConfig
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "bogus": 1}); !errors.As(err, &bad) || bad.Key != "bogus" {
		t.Fatalf("want ErrInvalidConfig{bogus}, got %v", err)
	}
}

func TestValidate_RejectedKeys(t *testing.T) {
	a := llmAdapter(t, okEnv())
	for _, k := range []string{"api_key", "session_id", "messages", "tools", "stream"} {
		if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", k: "x"}); err == nil {
			t.Errorf("key %q must be rejected", k)
		}
	}
}

func TestValidate_StructuredOutputEnum(t *testing.T) {
	a := llmAdapter(t, okEnv())
	for _, ok := range []string{"response_format", "ollama_format", "off"} {
		if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "structured_output": ok}); err != nil {
			t.Errorf("structured_output=%q should pass: %v", ok, err)
		}
	}
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "structured_output": "auto"}); err == nil {
		t.Error("structured_output=auto should fail (no auto in v1)")
	}
}

func TestValidate_TempAndMaxTokensTypes(t *testing.T) {
	a := llmAdapter(t, okEnv())
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "temperature": 0.2, "max_tokens": 100}); err != nil {
		t.Errorf("numeric temperature + integer max_tokens should pass: %v", err)
	}
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "temperature": "hot"}); err == nil {
		t.Error("non-numeric temperature should fail")
	}
}

func TestValidate_TLSInsecureBool(t *testing.T) {
	a := llmAdapter(t, okEnv())
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "tls_insecure": true}); err != nil {
		t.Errorf("tls_insecure=true should pass: %v", err)
	}
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "tls_insecure": "yes"}); err == nil {
		t.Error("non-bool tls_insecure should fail")
	}
}

func TestValidate_ProviderEnum(t *testing.T) {
	// Forward both keys so the enum acceptance (not key presence) is what we test.
	// provider=ollama is now a first-class transport value (Fix B); the ollama
	// transport is a local server so it needs NO key env.
	a := llmAdapter(t, map[string]string{"OPENAI_API_KEY": "sk-test", "GEMINI_API_KEY": "k"})
	for _, ok := range []string{"openai", "gemini", "ollama"} {
		if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "provider": ok}); err != nil {
			t.Errorf("provider=%q should pass: %v", ok, err)
		}
	}
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "provider": "bogus"}); err == nil {
		t.Error("provider=bogus must fail (not in the enum)")
	}
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "provider": 1}); err == nil {
		t.Error("non-string provider should fail")
	}
}

// TestValidate_CrossKeyGuard — Fix B: naming a NON-ollama transport via provider
// (openai or gemini) while also asking for the Ollama transport via
// structured_output:ollama_format is a contradiction → reject.
func TestValidate_CrossKeyGuard(t *testing.T) {
	a := llmAdapter(t, map[string]string{"OPENAI_API_KEY": "sk-test", "GEMINI_API_KEY": "k"})
	for _, p := range []string{"openai", "gemini"} {
		err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "provider": p, "structured_output": "ollama_format"})
		var bad *agent.ErrInvalidConfig
		if !errors.As(err, &bad) {
			t.Errorf("provider=%q + structured_output=ollama_format must be rejected as *ErrInvalidConfig, got %v", p, err)
		}
	}
	// provider:ollama + structured_output:ollama_format is consistent — must pass.
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "provider": "ollama", "structured_output": "ollama_format"}); err != nil {
		t.Errorf("provider=ollama + structured_output=ollama_format should pass (consistent): %v", err)
	}
}

func TestValidate_GeminiUsesGeminiKeyEnvByDefault(t *testing.T) {
	// provider: gemini relying on the default key env passes only when GEMINI_API_KEY
	// is forwarded (the presence check tracks the provider, matching buildReqConfig).
	withGemini := ir.RawConfig{"model": "m", "prompt": "hi", "provider": "gemini"}
	a := llmAdapter(t, map[string]string{"GEMINI_API_KEY": "k"})
	if err := a.ValidateConfig(withGemini); err != nil {
		t.Errorf("provider=gemini with GEMINI_API_KEY present should pass: %v", err)
	}
	// only OPENAI_API_KEY present → the gemini default (GEMINI_API_KEY) is absent → reject.
	b := llmAdapter(t, okEnv())
	if err := b.ValidateConfig(withGemini); err == nil {
		t.Error("provider=gemini must require GEMINI_API_KEY (or an explicit api_key_env)")
	}
}

// TestValidate_OllamaKeyOptional — Fix B: provider:ollama is a LOCAL server, so
// ValidateConfig must NOT require any key env in the allowlist.
func TestValidate_OllamaKeyOptional(t *testing.T) {
	a := llmAdapter(t, map[string]string{}) // empty allowlist
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "provider": "ollama"}); err != nil {
		t.Errorf("provider=ollama must pass with no key env (local server): %v", err)
	}
	// Back-compat: a bare structured_output:ollama_format (no provider) is also the
	// ollama transport (effectiveProvider) → key likewise optional.
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "structured_output": "ollama_format"}); err != nil {
		t.Errorf("structured_output=ollama_format must pass with no key env (ollama transport): %v", err)
	}
}

func TestValidate_GeminiCacheAcceptedOnGemini(t *testing.T) {
	a := llmAdapter(t, map[string]string{"GEMINI_API_KEY": "k"})
	err := a.ValidateConfig(ir.RawConfig{
		"provider": "gemini", "model": "gemini-2.5-pro", "prompt": "x",
		"gemini_cache": map[string]any{"mode": "explicit", "ttl": "600s"},
	})
	if err != nil {
		t.Errorf("explicit gemini_cache on provider: gemini must validate: %v", err)
	}
}

func TestValidate_GeminiCacheRejectedOffGemini(t *testing.T) {
	a := llmAdapter(t, map[string]string{"OPENAI_API_KEY": "k"})
	err := a.ValidateConfig(ir.RawConfig{
		"model": "gpt-x", "prompt": "x",
		"gemini_cache": map[string]any{"mode": "explicit"},
	})
	if err == nil {
		t.Fatal("explicit gemini_cache without provider: gemini must be rejected")
	}
	var ic *agent.ErrInvalidConfig
	if !errors.As(err, &ic) {
		t.Fatalf("want *agent.ErrInvalidConfig, got %T", err)
	}
}

func TestValidate_GeminiCacheBadShape(t *testing.T) {
	a := llmAdapter(t, map[string]string{"GEMINI_API_KEY": "k"})
	cases := []ir.RawConfig{
		{"provider": "gemini", "model": "m", "prompt": "x", "gemini_cache": "explicit"},                                    // not a map
		{"provider": "gemini", "model": "m", "prompt": "x", "gemini_cache": map[string]any{"mode": "weird"}},               // bad mode
		{"provider": "gemini", "model": "m", "prompt": "x", "gemini_cache": map[string]any{"mode": "explicit", "ttl": ""}}, // empty ttl
	}
	for i, with := range cases {
		if err := a.ValidateConfig(with); err == nil {
			t.Errorf("case %d: expected rejection for %v", i, with["gemini_cache"])
		}
	}
}

func TestValidate_MaxInlineBytes(t *testing.T) {
	a := llmAdapter(t, okEnv())
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "max_inline_bytes": 1024}); err != nil {
		t.Errorf("positive integer max_inline_bytes should pass: %v", err)
	}
	for _, bad := range []any{0, -1, 10.5, "big"} {
		if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "max_inline_bytes": bad}); err == nil {
			t.Errorf("max_inline_bytes=%v (%T) must fail", bad, bad)
		}
	}
}

func TestValidate_MissingAPIKey(t *testing.T) {
	a := llmAdapter(t, map[string]string{}) // no OPENAI_API_KEY forwarded
	var inv *agent.ErrInvalidConfig
	err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi"})
	if !errors.As(err, &inv) {
		t.Fatalf("want *agent.ErrInvalidConfig, got %T: %v", err, err)
	}
	if inv.Key != "api_key_env" {
		t.Errorf("ErrInvalidConfig.Key = %q, want %q", inv.Key, "api_key_env")
	}
}

func TestValidate_CustomAPIKeyEnvHonored(t *testing.T) {
	a := llmAdapter(t, map[string]string{"MY_KEY": "v"})
	if err := a.ValidateConfig(ir.RawConfig{"model": "m", "prompt": "hi", "api_key_env": "MY_KEY"}); err != nil {
		t.Errorf("api_key_env naming a present var should pass: %v", err)
	}
}

// TestValidateConfigToolLoopExemptsPrompt: a react with: (no prompt) passes the
// prompt-exempt variant, but the rejectedKeys guard (incl. "tools") still applies
// and model is still required (Task 3.3).
func TestValidateConfigToolLoopExemptsPrompt(t *testing.T) {
	a := llmAdapter(t, okEnv())
	// no prompt key — must pass the prompt-exempt variant:
	if err := a.ValidateConfigForToolLoopForTest(ir.RawConfig{"model": "m"}); err != nil {
		t.Fatalf("prompt-exempt validate rejected a valid react with:: %v", err)
	}
	// a "tools" with-key is still rejected:
	if err := a.ValidateConfigForToolLoopForTest(ir.RawConfig{"model": "m", "tools": []any{}}); err == nil {
		t.Fatal("tools with-key should still be rejected by the exempt variant")
	}
	// model is still required (a shared-common check, not the prompt one):
	if err := a.ValidateConfigForToolLoopForTest(ir.RawConfig{}); err == nil {
		t.Fatal("missing model should still fail the exempt variant")
	}
	// ValidateConfig (the agent: path) STILL requires prompt:
	if err := a.ValidateConfig(ir.RawConfig{"model": "m"}); err == nil {
		t.Fatal("ValidateConfig must still require prompt")
	}
}
