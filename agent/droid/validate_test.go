package droid_test

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/droid"
	"github.com/valbaudo/awf/ir"
)

func newWithKey(t *testing.T) *droid.Adapter {
	t.Helper()
	a, err := droid.New(droid.WithEnv(map[string]string{"FACTORY_API_KEY": "fk-test"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestValidateConfig_HappyPath(t *testing.T) {
	a := newWithKey(t)
	with := ir.RawConfig{
		"prompt": "do the thing", "model": "claude-opus-4-8", "reasoning_effort": "high",
		"autonomy": "skip", "system_prompt": "be terse",
		"enabled_tools": []any{"Read", "Edit"}, "disabled_tools": []any{"Execute"},
	}
	if err := a.ValidateConfig(with); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
}

func TestValidateConfig_MissingPrompt(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"model": "x"})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "prompt" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:prompt}", err)
	}
}

func TestValidateConfig_UnknownKey(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"prompt": "x", "bogus": 1})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "bogus" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:bogus}", err)
	}
}

func TestValidateConfig_SessionKeysRejected(t *testing.T) {
	a := newWithKey(t)
	for _, k := range []string{"session_id", "resume", "fork", "continue"} {
		err := a.ValidateConfig(ir.RawConfig{"prompt": "x", k: "v"})
		var reuse *droid.ErrSessionReuseAttempted
		if !errors.As(err, &reuse) || reuse.Key != k {
			t.Errorf("key %q: err = %v, want *ErrSessionReuseAttempted{Key:%q}", k, err, k)
		}
	}
}

func TestValidateConfig_BadReasoningEffort(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"prompt": "x", "reasoning_effort": "ludicrous"})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "reasoning_effort" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:reasoning_effort}", err)
	}
}

func TestValidateConfig_BadAutonomy(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"prompt": "x", "autonomy": "yolo"})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "autonomy" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:autonomy}", err)
	}
}

func TestValidateConfig_MissingAPIKey(t *testing.T) {
	a, _ := droid.New() // no FACTORY_API_KEY
	err := a.ValidateConfig(ir.RawConfig{"prompt": "x"})
	var miss *droid.ErrMissingAPIKey
	if !errors.As(err, &miss) {
		t.Fatalf("err = %v, want *droid.ErrMissingAPIKey", err)
	}
}

func TestValidateConfig_EnabledToolsWrongType(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"prompt": "x", "enabled_tools": "Read"})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "enabled_tools" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:enabled_tools}", err)
	}
}

func TestValidateConfig_EmptyPrompt(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"prompt": ""})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "prompt" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:prompt}", err)
	}
}

func TestValidateConfig_NonStringPrompt(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"prompt": 42})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "prompt" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:prompt}", err)
	}
}

func TestValidateConfig_EnabledToolsNonStringElement(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"prompt": "x", "enabled_tools": []any{"Read", 42}})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "enabled_tools" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:enabled_tools}", err)
	}
}

// newWithBYOKEnv builds an adapter whose env allowlist carries the named BYOK
// key but NO FACTORY_API_KEY — pure BYOK needs no Factory account.
func newWithBYOKEnv(t *testing.T, name, val string) *droid.Adapter {
	t.Helper()
	a, err := droid.New(droid.WithEnv(map[string]string{name: val}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestValidateConfig_BYOK_HappyPathNoFactoryKey(t *testing.T) {
	a := newWithBYOKEnv(t, "OPENROUTER_API_KEY", "sk-test") // no FACTORY_API_KEY
	with := ir.RawConfig{
		"prompt": "do the thing", "model": "anthropic/claude-3",
		"base_url": "https://gateway.example/v1", "api_key_env": "OPENROUTER_API_KEY",
		"provider": "openai", "tls_insecure": false,
	}
	if err := a.ValidateConfig(with); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
}

func TestValidateConfig_BYOK_APIKeyEnvVarAbsent(t *testing.T) {
	a := newWithBYOKEnv(t, "SOMETHING_ELSE", "v") // OPENROUTER_API_KEY not present
	err := a.ValidateConfig(ir.RawConfig{
		"prompt": "x", "model": "anthropic/claude-3",
		"base_url": "https://gateway.example/v1", "api_key_env": "OPENROUTER_API_KEY",
	})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "api_key_env" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:api_key_env}", err)
	}
}

func TestValidateConfig_BYOK_APIKeyEnvMissing(t *testing.T) {
	a := newWithBYOKEnv(t, "OPENROUTER_API_KEY", "sk-test")
	err := a.ValidateConfig(ir.RawConfig{
		"prompt": "x", "model": "anthropic/claude-3",
		"base_url": "https://gateway.example/v1", // no api_key_env
	})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "api_key_env" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:api_key_env}", err)
	}
}

func TestValidateConfig_BYOK_ModelMissing(t *testing.T) {
	a := newWithBYOKEnv(t, "OPENROUTER_API_KEY", "sk-test")
	err := a.ValidateConfig(ir.RawConfig{
		"prompt": "x", "base_url": "https://gateway.example/v1", "api_key_env": "OPENROUTER_API_KEY",
	})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "model" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:model}", err)
	}
}

func TestValidateConfig_BYOK_ModelWhitespace(t *testing.T) {
	a := newWithBYOKEnv(t, "OPENROUTER_API_KEY", "sk-test")
	for _, m := range []string{"claude 3", "claude\n3", "claude\t3", " claude3"} {
		err := a.ValidateConfig(ir.RawConfig{
			"prompt": "x", "model": m,
			"base_url": "https://gateway.example/v1", "api_key_env": "OPENROUTER_API_KEY",
		})
		var bad *agent.ErrInvalidConfig
		if !errors.As(err, &bad) || bad.Key != "model" {
			t.Errorf("model %q: err = %v, want *agent.ErrInvalidConfig{Key:model}", m, err)
		}
	}
}

func TestValidateConfig_BYOK_ModelPunctuationAllowed(t *testing.T) {
	a := newWithBYOKEnv(t, "OPENROUTER_API_KEY", "sk-test")
	for _, m := range []string{"bedrock/anthropic.claude-3", "ollama/llama3:8b", "gpt-4o-mini"} {
		with := ir.RawConfig{
			"prompt": "x", "model": m,
			"base_url": "https://gateway.example/v1", "api_key_env": "OPENROUTER_API_KEY",
		}
		if err := a.ValidateConfig(with); err != nil {
			t.Errorf("model %q: ValidateConfig = %v, want nil", m, err)
		}
	}
}

// An empty base_url must NOT trigger BYOK mode: it falls through to the legacy
// path and (with no FACTORY_API_KEY) yields *ErrMissingAPIKey.
func TestValidateConfig_EmptyBaseURLFallsThroughToLegacy(t *testing.T) {
	a, _ := droid.New() // no FACTORY_API_KEY
	err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "base_url": ""})
	var miss *droid.ErrMissingAPIKey
	if !errors.As(err, &miss) {
		t.Fatalf("err = %v, want *droid.ErrMissingAPIKey", err)
	}
}

func TestValidateConfig_InlineAPIKeyRejected(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"prompt": "x", "api_key": "sk-leak"})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "api_key" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:api_key}", err)
	}
}

func TestValidateConfig_TLSInsecureNonBool(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"prompt": "x", "tls_insecure": "yes"})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "tls_insecure" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:tls_insecure}", err)
	}
}

func TestValidateConfig_ProviderNonString(t *testing.T) {
	err := newWithKey(t).ValidateConfig(ir.RawConfig{"prompt": "x", "provider": 42})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "provider" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:provider}", err)
	}
}

func TestValidateConfig_ProviderAnyNonEmptyAccepted(t *testing.T) {
	a := newWithKey(t) // FACTORY_API_KEY present, no base_url → legacy path
	for _, p := range []string{"openai", "anthropic", "made-up-provider"} {
		if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "provider": p}); err != nil {
			t.Errorf("provider %q: ValidateConfig = %v, want nil (no enum)", p, err)
		}
	}
}
