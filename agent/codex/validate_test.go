package codex_test

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/codex"
	"github.com/valbaudo/awf/ir"
)

func codexAdapter(t *testing.T) *codex.Adapter {
	t.Helper()
	a, err := codex.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestValidate_RequiresPrompt(t *testing.T) {
	a := codexAdapter(t)
	var bad *agent.ErrInvalidConfig
	if err := a.ValidateConfig(ir.RawConfig{}); !errors.As(err, &bad) || bad.Key != "prompt" {
		t.Fatalf("ValidateConfig({}) = %v, want ErrInvalidConfig{Key:prompt}", err)
	}
}

func TestValidate_EmptyPromptRejected(t *testing.T) {
	a := codexAdapter(t)
	if err := a.ValidateConfig(ir.RawConfig{"prompt": ""}); err == nil {
		t.Fatal("empty prompt should fail")
	}
}

func TestValidate_UnknownKey(t *testing.T) {
	a := codexAdapter(t)
	var bad *agent.ErrInvalidConfig
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "bogus": 1}); !errors.As(err, &bad) || bad.Key != "bogus" {
		t.Fatalf("want ErrInvalidConfig{Key:bogus}, got %v", err)
	}
}

func TestValidate_SessionKeysRejected(t *testing.T) {
	a := codexAdapter(t)
	for _, k := range []string{"resume", "fork", "last", "session_id", "continue"} {
		var reuse *codex.ErrSessionReuseAttempted
		if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", k: "v"}); !errors.As(err, &reuse) {
			t.Errorf("key %q: want ErrSessionReuseAttempted, got %v", k, err)
		}
	}
}

func TestValidate_SandboxEnum(t *testing.T) {
	a := codexAdapter(t)
	for _, ok := range []string{"read-only", "workspace-write", "danger-full-access"} {
		if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "sandbox": ok}); err != nil {
			t.Errorf("sandbox=%q should pass: %v", ok, err)
		}
	}
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "sandbox": "yolo"}); err == nil {
		t.Error("sandbox=yolo should fail (not in enum)")
	}
}

func TestValidate_ReasoningEffortEnum(t *testing.T) {
	a := codexAdapter(t)
	// All SIX values codex v0.131.0 accepts (verified against the binary's own
	// config validation: "expected one of none, minimal, low, medium, high, xhigh").
	for _, ok := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "reasoning_effort": ok}); err != nil {
			t.Errorf("reasoning_effort=%q should pass: %v", ok, err)
		}
	}
	// A non-enum value (TOML-unsafe even after shell-quoting) must be rejected at
	// validate time, never reaching the `-c model_reasoning_effort=` flag.
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "reasoning_effort": `high"; x`}); err == nil {
		t.Error("reasoning_effort with a non-enum value should fail")
	}
}

func TestValidate_ModelType(t *testing.T) {
	a := codexAdapter(t)
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "model": "gpt-5-codex"}); err != nil {
		t.Errorf("model string should pass: %v", err)
	}
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "model": 42}); err == nil {
		t.Error("model non-string should fail")
	}
}
