package cli_test

import (
	"testing"

	"github.com/valbaudo/awf/cli"
)

func TestRunner_AgentEnvField_ZeroValue(t *testing.T) {
	var r cli.Runner
	if r.AgentEnv != nil {
		t.Errorf("AgentEnv default = %v, want nil", r.AgentEnv)
	}
}

func TestRunner_AgentEnvField_Populated(t *testing.T) {
	r := cli.Runner{AgentEnv: []string{"ANTHROPIC_API_KEY"}}
	if len(r.AgentEnv) != 1 || r.AgentEnv[0] != "ANTHROPIC_API_KEY" {
		t.Errorf("AgentEnv = %v", r.AgentEnv)
	}
}
