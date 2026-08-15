package claude

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

func TestValidateConfig_HappyPath_MinimalPrompt(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "do the thing"})
	if err != nil {
		t.Errorf("ValidateConfig: %v", err)
	}
}

func TestValidateConfig_HappyPath_AllOptionalKeys(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	err := a.ValidateConfig(ir.RawConfig{
		"prompt":         "p",
		"model":          "claude-opus-4-7",
		"effort":         "max",
		"max_turns":      10,
		"system_prompt":  "you are X",
		"allowed_tools":  []any{"Bash", "Read"},
		"bare":           true,
		"max_budget_usd": 5.0,
	})
	if err != nil {
		t.Errorf("ValidateConfig: %v", err)
	}
}

// F19: a non-string entry in allowed_tools must be rejected element-wise, not
// silently dropped (the old behavior filtered it out at launch time).
func TestClaude_ToolsElementNonStringRejected(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "allowed_tools": []any{"Bash", 1}})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) || bad.Key != "allowed_tools" {
		t.Fatalf("err = %v, want *agent.ErrInvalidConfig{Key:allowed_tools}", err)
	}
}

func TestValidateConfig_EffortBareWord(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	// transport-safety only: any bare word passes (claude owns its tier
	// vocabulary; a frozen enum rejected codex's post-v0.131.0 max/ultra —
	// enum removed 2026-08-15)
	for _, ok := range []string{"low", "medium", "high", "xhigh", "max", "ultra", "somefuturetier"} {
		if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "effort": ok}); err != nil {
			t.Errorf("effort=%q should pass: %v", ok, err)
		}
	}
	// anything but a bare word is rejected before it reaches the shell-quoted
	// command line
	for _, bad := range []string{`high"; rm -rf /`, "High", "high effort", ""} {
		if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "effort": bad}); err == nil {
			t.Errorf("effort=%q should fail (not a bare word)", bad)
		}
	}
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "x", "effort": 1}); err == nil {
		t.Error("effort non-string should fail")
	}
}

func TestValidateConfig_MissingPrompt_Rejects(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	err := a.ValidateConfig(ir.RawConfig{"model": "claude-opus"})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v; want *agent.ErrInvalidConfig", err)
	}
	if bad.Key != "prompt" {
		t.Errorf("Key = %q; want %q", bad.Key, "prompt")
	}
}

func TestValidateConfig_PromptWrongType_Rejects(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": 123})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v; want *agent.ErrInvalidConfig", err)
	}
}

func TestValidateConfig_UnknownKey_Rejects(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "verbose": true})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v; want *agent.ErrInvalidConfig", err)
	}
	if bad.Key != "verbose" {
		t.Errorf("Key = %q; want %q", bad.Key, "verbose")
	}
}

func TestValidateConfig_SessionID_Rejects(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "session_id": "abc"})
	var sess *ErrSessionReuseAttempted
	if !errors.As(err, &sess) {
		t.Fatalf("err = %v; want *ErrSessionReuseAttempted", err)
	}
	if sess.Key != "session_id" {
		t.Errorf("Key = %q", sess.Key)
	}
}

func TestValidateConfig_Continue_Rejects(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "continue": true})
	var sess *ErrSessionReuseAttempted
	if !errors.As(err, &sess) {
		t.Fatalf("err = %v; want *ErrSessionReuseAttempted", err)
	}
}

func TestValidateConfig_Resume_Rejects(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "resume": "xyz"})
	var sess *ErrSessionReuseAttempted
	if !errors.As(err, &sess) {
		t.Fatalf("err = %v; want *ErrSessionReuseAttempted", err)
	}
}

func TestValidateConfig_BareTrue_NoAPIKey_Rejects(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"OTHER": "v"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "bare": true})
	var bare *ErrBareRequiresAPIKey
	if !errors.As(err, &bare) {
		t.Fatalf("err = %v; want *ErrBareRequiresAPIKey", err)
	}
}

func TestValidateConfig_BareTrue_WithAuthToken_OK(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"ANTHROPIC_AUTH_TOKEN": "x"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "bare": true})
	if err != nil {
		t.Errorf("ValidateConfig: %v (ANTHROPIC_AUTH_TOKEN should satisfy bare requirement)", err)
	}
}

func TestValidateConfig_BareFalse_NoAPIKey_OK(t *testing.T) {
	a, _ := New(WithEnv(map[string]string{"OTHER": "v"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "bare": false})
	if err != nil {
		t.Errorf("ValidateConfig: %v (bare:false opts out of API-key requirement)", err)
	}
}

func TestValidateConfig_BareDefault_NoAPIKey_Rejects(t *testing.T) {
	// AWF default for bare is true per decision 9. When `bare` is not
	// supplied, the validator treats it as true.
	a, _ := New(WithEnv(map[string]string{"OTHER": "v"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p"})
	var bare *ErrBareRequiresAPIKey
	if !errors.As(err, &bare) {
		t.Fatalf("err = %v; want *ErrBareRequiresAPIKey (bare defaults to true per decision 9)", err)
	}
}
