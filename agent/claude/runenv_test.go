package claude_test

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claude"
)

func TestApplyPerRunConfigEnv_SetsConfigAndXDG(t *testing.T) {
	env := map[string]string{"ANTHROPIC_API_KEY": "sk"}
	claude.ApplyPerRunConfigEnv(env, agent.AgentInvocation{SessionConfigDir: "/work/.awf/claude-session/r1"})

	want := map[string]string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"CLAUDE_CONFIG_DIR": "/work/.awf/claude-session/r1",
		"XDG_STATE_HOME":    "/work/.awf/claude-session/r1/xdg-state",
		"XDG_CACHE_HOME":    "/work/.awf/claude-session/r1/xdg-cache",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, env[k], v)
		}
	}
	if env["ANTHROPIC_API_KEY"] != "sk" {
		t.Error("must not drop pre-existing env keys")
	}
	// HOME / XDG_DATA_HOME must stay UNSET: claude's versioned binary lives under
	// $XDG_DATA_HOME — relocating it (or HOME) would break binary resolution.
	for _, k := range []string{"HOME", "XDG_DATA_HOME"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s must NOT be set (would relocate the versioned claude binary dir)", k)
		}
	}
}

func TestApplyPerRunConfigEnv_EmptyConfigDir_OnlyHygiene(t *testing.T) {
	env := map[string]string{}
	claude.ApplyPerRunConfigEnv(env, agent.AgentInvocation{}) // SessionConfigDir empty

	if env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" {
		t.Error("hygiene toggle must always be set")
	}
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s must be absent when SessionConfigDir is empty", k)
		}
	}
}
