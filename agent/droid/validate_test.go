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
