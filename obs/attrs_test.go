package obs

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
)

func TestStepAttributesAgentWithMetrics(t *testing.T) {
	exit := 0
	nc := engine.NodeCompletedData{
		Outcome:  "ok",
		ExitCode: &exit,
		Usage: &agent.MetricSet{
			Cost:   agent.MetricCost{Total: 0.04, Source: agent.CostSourceReported},
			Tokens: agent.MetricTokens{Input: 100, Output: 50, CacheReadInput: 10},
			Turns:  3,
		},
	}
	attrs := stepAttributes("triage", "agent", nc)

	if attrs[AttrNodePath] != "triage" || attrs[AttrNodeKind] != "agent" || attrs[AttrNodeOutcome] != "ok" {
		t.Errorf("core attrs wrong: %+v", attrs)
	}
	if attrs[AttrCostUSD] != 0.04 || attrs[AttrCostSource] != "reported" {
		t.Errorf("cost attrs wrong: %+v", attrs)
	}
	if attrs[AttrGenAIInputTokens] != int64(100) || attrs[AttrGenAIOutputTokens] != int64(50) {
		t.Errorf("token attrs wrong: %+v", attrs)
	}
	if attrs[AttrAgentTurns] != int64(3) {
		t.Errorf("turns wrong: %+v", attrs)
	}
}

func TestStepAttributesCodeOmitsCost(t *testing.T) {
	exit := 0
	nc := engine.NodeCompletedData{Outcome: "ok", ExitCode: &exit}
	attrs := stepAttributes("build", "code", nc)
	if _, ok := attrs[AttrCostUSD]; ok {
		t.Errorf("code step must not carry %s", AttrCostUSD)
	}
	if attrs[AttrExitCode] != int64(0) {
		t.Errorf("code step should carry exit_code, got %+v", attrs)
	}
}
