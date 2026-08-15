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

func TestValidate_EffortBareWord(t *testing.T) {
	a := codexAdapter(t)
	// Any bare word passes: codex owns the tier vocabulary. The enum this
	// replaced (frozen at v0.131.0's six values) rejected VALID tiers codex
	// added later (max/ultra in v0.146.0) — 2026-08-15.
	for _, ok := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "somefuturetier"} {
		if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "effort": ok}); err != nil {
			t.Errorf("effort=%q should pass: %v", ok, err)
		}
	}
	// A non-bare-word value (TOML/shell-unsafe even after shell-quoting) must
	// be rejected at validate time, never reaching the
	// `-c model_reasoning_effort=` flag.
	for _, bad := range []string{`high"; x`, "High", "high effort", ""} {
		if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "effort": bad}); err == nil {
			t.Errorf("effort=%q should fail (not a bare word)", bad)
		}
	}
}

// F12: reasoning_effort was codex's original with-key name; effort is now the
// sole canonical name (matching anthropic/claude-code). The old key must not
// silently vanish or fall through to the generic unknown-key message — it gets
// a specific rename pointer, and KeyUnknown:true so the run-start with:-config
// guard (U1) surfaces it pre-spend.
func TestCodex_EffortRenamed(t *testing.T) {
	a := codexAdapter(t)
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "effort": "high"}); err != nil {
		t.Errorf("effort=high should pass: %v", err)
	}
	var bad *agent.ErrInvalidConfig
	err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "reasoning_effort": "high"})
	if !errors.As(err, &bad) {
		t.Fatalf("reasoning_effort: err = %v, want *agent.ErrInvalidConfig", err)
	}
	if bad.Key != "reasoning_effort" {
		t.Errorf("bad.Key = %q, want %q", bad.Key, "reasoning_effort")
	}
	if bad.Reason != "renamed to effort" {
		t.Errorf("bad.Reason = %q, want %q", bad.Reason, "renamed to effort")
	}
	if !bad.KeyUnknown {
		t.Error("bad.KeyUnknown = false, want true")
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
