package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/valbaudo/awf/cli"
)

func TestPrintUsageListsReadCommands(t *testing.T) {
	var b bytes.Buffer
	cli.PrintUsageForTest(&b)
	for _, cmd := range []string{"ls", "inspect", "trace"} {
		if !strings.Contains(b.String(), cmd) {
			t.Errorf("printUsage missing %q:\n%s", cmd, b.String())
		}
	}
}

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
