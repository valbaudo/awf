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
	const secret = "sk-MUST-NOT-LEAK"
	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     "anthropic/claude-code",
		With:     map[string]any{"prompt": "hello"},
		Env:      agent.SecretEnv{"ANTHROPIC_API_KEY": secret},
	}
	b, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Locks the json:"-" guarantee on Env: secret values must NEVER appear in
	// the marshaled bytes. The engine's state log is JSON; this is what
	// keeps secrets out of the journal.
	if strings.Contains(string(b), secret) {
		t.Fatalf("JSON marshal leaked secret value: %s", b)
	}
	var got agent.AgentInvocation
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.NodePath != inv.NodePath {
		t.Errorf("NodePath = %q, want %q", got.NodePath, inv.NodePath)
	}
	if got.Uses != inv.Uses {
		t.Errorf("Uses = %q, want %q", got.Uses, inv.Uses)
	}
	if !reflect.DeepEqual(got.With, inv.With) {
		t.Errorf("With not preserved (got len=%d, want len=%d)", len(got.With), len(inv.With))
	}
	if len(got.Env) != 0 {
		t.Errorf("Env survived JSON roundtrip — expected empty (json:\"-\") but got %d entries", len(got.Env))
	}
}

func TestSecretEnv_RedactsInStandardFormatters(t *testing.T) {
	const secret = "sk-NEVER-LEAK-THIS"
	e := agent.SecretEnv{"ANTHROPIC_API_KEY": secret, "OTHER": "also-secret"}

	// %+v is included: Go's printer consults Stringer on a defined map type
	// even with the +flag, so redaction must hold there too. Verified by
	// TestSecretEnv_RedactsInsideStruct against an AgentInvocation field.
	for _, verb := range []string{"%v", "%s", "%q", "%#v", "%+v"} {
		got := fmt.Sprintf(verb, e)
		if strings.Contains(got, secret) {
			t.Errorf("verb %q leaked secret value: %s", verb, got)
		}
		if strings.Contains(got, "also-secret") {
			t.Errorf("verb %q leaked second secret: %s", verb, got)
		}
	}
}

func TestSecretEnv_RedactsInsideStruct(t *testing.T) {
	// %+v on a struct containing a SecretEnv field MUST also redact — this is
	// the scenario the SecretEnv doc-comment's "even %+v is safe" claim rests
	// on. If a future Go release changes Stringer dispatch on defined map types
	// inside struct fields, this test fails and the doc-comment must be revised.
	const secret = "sk-IN-STRUCT"
	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Env:      agent.SecretEnv{"ANTHROPIC_API_KEY": secret},
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		got := fmt.Sprintf(verb, inv)
		if strings.Contains(got, secret) {
			t.Errorf("verb %q on AgentInvocation leaked Env value: %s", verb, got)
		}
	}
}

func TestAgentResult_OutputIsMap(t *testing.T) {
	r := agent.AgentResult{
		Output:   map[string]any{"verdict": "pass", "score": 5.0},
		ExitCode: 0,
		Metrics: agent.MetricSet{
			Cost:   agent.MetricCost{USD: 0.0125, Source: agent.CostSourceReported},
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
