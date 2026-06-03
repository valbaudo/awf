package goose_test

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/goose"
	"github.com/valbaudo/awf/ir"
)

func gooseAdapterEnv(t *testing.T, env map[string]string) *goose.Adapter {
	t.Helper()
	a, err := goose.New(goose.WithEnv(env))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestValidate_RequiresPrompt(t *testing.T) {
	a := gooseAdapterEnv(t, nil)
	var bad *agent.ErrInvalidConfig
	if err := a.ValidateConfig(ir.RawConfig{}); !errors.As(err, &bad) || bad.Key != "prompt" {
		t.Fatalf("ValidateConfig({}) = %v, want ErrInvalidConfig{Key:prompt}", err)
	}
}

func TestValidate_UnknownKey(t *testing.T) {
	a := gooseAdapterEnv(t, nil)
	var bad *agent.ErrInvalidConfig
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "bogus": 1}); !errors.As(err, &bad) || bad.Key != "bogus" {
		t.Fatalf("want ErrInvalidConfig{Key:bogus}, got %v", err)
	}
}

func TestValidate_SessionKeysRejected(t *testing.T) {
	a := gooseAdapterEnv(t, nil)
	for _, k := range []string{"resume", "name", "session_id", "path", "fork", "interactive", "continue"} {
		var reuse *goose.ErrSessionReuseAttempted
		if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", k: "v"}); !errors.As(err, &reuse) {
			t.Errorf("key %q: want ErrSessionReuseAttempted, got %v", k, err)
		}
	}
}

func TestValidate_MaxTurnsType(t *testing.T) {
	a := gooseAdapterEnv(t, nil)
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "max_turns": float64(50)}); err != nil {
		t.Errorf("max_turns=50 should pass: %v", err)
	}
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "max_turns": "lots"}); err == nil {
		t.Error("max_turns string should fail")
	}
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "max_turns": float64(0)}); err == nil {
		t.Error("max_turns=0 should fail (must be positive)")
	}
}

func TestValidate_ProviderConditionalAuth(t *testing.T) {
	// GOOSE_PROVIDER unset → no gate (claude-code in config.yaml; adapter can't see it).
	if err := gooseAdapterEnv(t, nil).ValidateConfig(ir.RawConfig{"prompt": "x"}); err != nil {
		t.Errorf("no GOOSE_PROVIDER → no gate, got %v", err)
	}
	// claude-code → no key required.
	if err := gooseAdapterEnv(t, map[string]string{"GOOSE_PROVIDER": "claude-code"}).ValidateConfig(ir.RawConfig{"prompt": "x"}); err != nil {
		t.Errorf("claude-code needs no key, got %v", err)
	}
	// anthropic without key → ErrMissingAPIKey.
	var miss *goose.ErrMissingAPIKey
	if err := gooseAdapterEnv(t, map[string]string{"GOOSE_PROVIDER": "anthropic"}).ValidateConfig(ir.RawConfig{"prompt": "x"}); !errors.As(err, &miss) || miss.Key != "ANTHROPIC_API_KEY" {
		t.Errorf("anthropic w/o key → want ErrMissingAPIKey{ANTHROPIC_API_KEY}, got %v", err)
	}
	// anthropic with key → pass.
	if err := gooseAdapterEnv(t, map[string]string{"GOOSE_PROVIDER": "anthropic", "ANTHROPIC_API_KEY": "sk"}).ValidateConfig(ir.RawConfig{"prompt": "x"}); err != nil {
		t.Errorf("anthropic w/ key should pass, got %v", err)
	}
	// openai without key → ErrMissingAPIKey{OPENAI_API_KEY}.
	if err := gooseAdapterEnv(t, map[string]string{"GOOSE_PROVIDER": "openai"}).ValidateConfig(ir.RawConfig{"prompt": "x"}); !errors.As(err, &miss) || miss.Key != "OPENAI_API_KEY" {
		t.Errorf("openai w/o key → want ErrMissingAPIKey{OPENAI_API_KEY}, got %v", err)
	}
	// unknown provider → no gate.
	if err := gooseAdapterEnv(t, map[string]string{"GOOSE_PROVIDER": "ollama"}).ValidateConfig(ir.RawConfig{"prompt": "x"}); err != nil {
		t.Errorf("unknown provider → no gate, got %v", err)
	}
}
