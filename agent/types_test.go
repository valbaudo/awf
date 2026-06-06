package agent_test

import (
	"encoding/json"
	"errors"
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

func TestAgentOutcome_HappyPath(t *testing.T) {
	o := agent.AgentOutcome{
		Result: agent.AgentResult{
			Output: map[string]any{"verdict": "pass"},
			Metrics: agent.MetricSet{
				Cost: agent.MetricCost{USD: 0.01, Source: agent.CostSourceReported},
			},
		},
		Err: nil,
	}
	if o.Err != nil {
		t.Errorf("Err should be nil on happy path")
	}
	if o.Result.Output["verdict"] != "pass" {
		t.Errorf("Output[verdict] = %v", o.Result.Output["verdict"])
	}
}

func TestAgentOutcome_FailurePath(t *testing.T) {
	cause := errors.New("transport bad")
	o := agent.AgentOutcome{
		Err: &agent.ErrAgentLaunch{Cause: cause},
	}
	var launch *agent.ErrAgentLaunch
	if !errors.As(o.Err, &launch) {
		t.Fatalf("Err = %v; want *ErrAgentLaunch", o.Err)
	}
	if !errors.Is(launch.Cause, cause) {
		t.Errorf("Cause not preserved")
	}
}

func TestAgentResult_TranscriptFieldNotJSONed(t *testing.T) {
	r := agent.AgentResult{
		Output:     map[string]any{"final": "x"},
		ExitCode:   0,
		Transcript: agent.ThreadTurn{User: "clean-prompt", Assistant: "verbatim-final"},
	}
	// Field is addressable verbatim...
	if r.Transcript.User != "clean-prompt" || r.Transcript.Assistant != "verbatim-final" {
		t.Fatalf("Transcript = %+v, want {clean-prompt verbatim-final}", r.Transcript)
	}
	// ...but json:"-" keeps it out of the journal (like Env SecretEnv).
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "transcript") ||
		strings.Contains(string(b), "verbatim-final") ||
		strings.Contains(string(b), "clean-prompt") {
		t.Errorf("AgentResult JSON %q leaked Transcript; want json:\"-\"", b)
	}
}

func TestCaps_ThreadedZeroValueAndTag(t *testing.T) {
	var c agent.Caps // zero value
	if c.Threaded {
		t.Errorf("zero-value Caps.Threaded = true, want false")
	}
	c.Threaded = true
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"threaded":true`) {
		t.Errorf("Caps JSON %q missing `\"threaded\":true`", b)
	}
	// omitempty: false Threaded is omitted.
	b2, _ := json.Marshal(agent.Caps{})
	if strings.Contains(string(b2), "threaded") {
		t.Errorf("zero Caps serialized %q, want threaded omitted (omitempty)", b2)
	}
}
