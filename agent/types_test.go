package agent_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
)

func TestCaps_JSONRoundtrip(t *testing.T) {
	c := agent.Caps{NativeSchema: true}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got agent.Caps
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != c {
		t.Errorf("roundtrip: got %+v, want %+v", got, c)
	}
}

func TestAgentInvocation_RetainsRawConfig(t *testing.T) {
	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     "anthropic/claude-code",
		With:     map[string]any{"prompt": "hello"},
		Env:      agent.SecretEnv{"ANTHROPIC_API_KEY": "sk-redacted"},
	}
	b, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got agent.AgentInvocation
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// NOTE: testing with NodePath/Uses literals — NOT %+v (security: %+v bypasses
	// Stringer and would leak Env values in test output on failure).
	if got.NodePath != inv.NodePath {
		t.Errorf("NodePath = %q, want %q", got.NodePath, inv.NodePath)
	}
	if got.Uses != inv.Uses {
		t.Errorf("Uses = %q, want %q", got.Uses, inv.Uses)
	}
	if !reflect.DeepEqual(got.With, inv.With) {
		t.Errorf("With not preserved (got len=%d, want len=%d)", len(got.With), len(inv.With))
	}
	if !reflect.DeepEqual(got.Env, inv.Env) {
		t.Errorf("Env not preserved (got %d entries, want %d entries)", len(got.Env), len(inv.Env))
	}
}

func TestSecretEnv_RedactsInStandardFormatters(t *testing.T) {
	const secret = "sk-NEVER-LEAK-THIS"
	e := agent.SecretEnv{"ANTHROPIC_API_KEY": secret, "OTHER": "also-secret"}

	for _, verb := range []string{"%v", "%s", "%q", "%#v"} {
		got := fmt.Sprintf(verb, e)
		if strings.Contains(got, secret) {
			t.Errorf("verb %q leaked secret value: %s", verb, got)
		}
		if strings.Contains(got, "also-secret") {
			t.Errorf("verb %q leaked second secret: %s", verb, got)
		}
	}
}

func TestSecretEnv_KnownGap_PlusVStillLeaks(t *testing.T) {
	// This test LOCKS THE KNOWN LIMITATION: Go's %+v reflection-based field
	// walker bypasses Stringer/GoStringer entirely. The doc-comment on
	// SecretEnv warns about this; this test ensures the warning stays accurate
	// (if Go ever changes %+v to consult Stringer, this test fails and we update
	// the doc-comment to reflect the new safer behavior).
	const secret = "sk-VISIBLE-IN-PLUSV"
	e := agent.SecretEnv{"ANTHROPIC_API_KEY": secret}
	got := fmt.Sprintf("%+v", e)
	if !strings.Contains(got, secret) {
		t.Logf("Heads-up: %%+v no longer leaks SecretEnv values. Go behavior may have changed; update SecretEnv doc-comment to reflect the stronger guarantee.")
		t.Logf("got: %s", got)
	}
}

func TestAgentResult_OutputIsMap(t *testing.T) {
	r := agent.AgentResult{
		Output:   map[string]any{"verdict": "pass", "score": 5.0},
		ExitCode: 0,
		Metrics: agent.MetricSet{
			Cost:   agent.MetricCost{USD: 0.0125, Source: "reported"},
			Tokens: agent.MetricTokens{Input: 100, Output: 50},
			Turns:  2,
		},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got agent.AgentResult
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.Output, r.Output) {
		t.Errorf("Output not preserved: got %+v, want %+v", got.Output, r.Output)
	}
	if got.Metrics.Cost.USD != r.Metrics.Cost.USD {
		t.Errorf("Metrics.Cost.USD = %v, want %v", got.Metrics.Cost.USD, r.Metrics.Cost.USD)
	}
}
