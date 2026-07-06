package goose_test

import (
	"errors"
	"strings"
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

// TestGoose_ProviderWithKey_Accepted (F34): with:provider is a known key that
// passes ValidateConfig (paired with an OPENAI_API_KEY so the auth gate, which
// now resolves the SAME with:provider, doesn't reject it for an unrelated reason).
func TestGoose_ProviderWithKey_Accepted(t *testing.T) {
	a := gooseAdapterEnv(t, map[string]string{"OPENAI_API_KEY": "sk"})
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "provider": "openai"}); err != nil {
		t.Errorf("provider:openai should be accepted, got %v", err)
	}
}

func TestGoose_ProviderWithKey_MustBeNonEmptyString(t *testing.T) {
	a := gooseAdapterEnv(t, nil)
	var bad *agent.ErrInvalidConfig
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "provider": 123}); !errors.As(err, &bad) || bad.Key != "provider" {
		t.Errorf("provider:123 (non-string) → want ErrInvalidConfig{Key:provider}, got %v", err)
	}
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "provider": ""}); !errors.As(err, &bad) || bad.Key != "provider" {
		t.Errorf("provider:\"\" (empty) → want ErrInvalidConfig{Key:provider}, got %v", err)
	}
}

// TestGoose_ProviderWithKey_BeatsEnv_InAuthGate (F34): with:provider takes
// precedence over the adapter's inherited GOOSE_PROVIDER env in the auth-gate
// resolution — the gate must check the key the CHOSEN provider (with:provider)
// needs, not the one the env var names, and must do so even when the env's
// provider would have needed a DIFFERENT (also-missing) key.
func TestGoose_ProviderWithKey_BeatsEnv_InAuthGate(t *testing.T) {
	// env says anthropic (no ANTHROPIC_API_KEY); with:provider says openai (no
	// OPENAI_API_KEY either) → must fail on OPENAI_API_KEY, proving with:provider
	// (not the env) drove the resolution.
	a := gooseAdapterEnv(t, map[string]string{"GOOSE_PROVIDER": "anthropic"})
	var miss *goose.ErrMissingAPIKey
	err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "provider": "openai"})
	if !errors.As(err, &miss) || miss.Key != "OPENAI_API_KEY" {
		t.Fatalf("with:provider=openai over env GOOSE_PROVIDER=anthropic → want ErrMissingAPIKey{OPENAI_API_KEY}, got %v", err)
	}
	// Now satisfy openai's key → passes despite env's anthropic key being absent.
	a2 := gooseAdapterEnv(t, map[string]string{"GOOSE_PROVIDER": "anthropic", "OPENAI_API_KEY": "sk"})
	if err := a2.ValidateConfig(ir.RawConfig{"prompt": "x", "provider": "openai"}); err != nil {
		t.Errorf("with:provider=openai + OPENAI_API_KEY present should pass, got %v", err)
	}
}

// TestGoose_MissingAPIKeyMessage_NamesResolvedProvider_NotGooseProviderEnv
// (final-review fix, post-F34): ErrMissingAPIKey's message used to always
// blame "the configured GOOSE_PROVIDER" — wrong once with:provider (which
// BEATS the env) is the one that actually selected the provider. The message
// must name the resolved provider generically instead of implying it came
// from the GOOSE_PROVIDER env var, and must still surface the correct key.
func TestGoose_MissingAPIKeyMessage_NamesResolvedProvider_NotGooseProviderEnv(t *testing.T) {
	// with:provider=openai selects openai even though env says GOOSE_PROVIDER=anthropic.
	a := gooseAdapterEnv(t, map[string]string{"GOOSE_PROVIDER": "anthropic"})
	err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "provider": "openai"})
	var miss *goose.ErrMissingAPIKey
	if !errors.As(err, &miss) {
		t.Fatalf("err = %v, want *goose.ErrMissingAPIKey", err)
	}
	msg := miss.Error()
	if miss.Provider != "openai" {
		t.Errorf("miss.Provider = %q, want %q", miss.Provider, "openai")
	}
	if !strings.Contains(msg, `"openai"`) {
		t.Errorf("message does not name the resolved provider %q: %s", "openai", msg)
	}
	if strings.Contains(msg, "the configured GOOSE_PROVIDER") {
		t.Errorf("message wrongly blames GOOSE_PROVIDER when with:provider selected it: %s", msg)
	}
	if !strings.Contains(msg, "OPENAI_API_KEY") {
		t.Errorf("message missing the required key: %s", msg)
	}
}
