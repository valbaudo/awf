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
