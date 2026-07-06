package droid

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

func TestResultFromCompletion_SuccessNoSchema_NilOutput(t *testing.T) {
	ev, err := parseStreamEvent([]byte(`{"type":"completion","finalText":"all done","numTurns":2,"durationMs":10,"session_id":"s1","usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":1,"cache_creation_input_tokens":2}}`))
	if err != nil {
		t.Fatalf("parseStreamEvent: %v", err)
	}
	res, eerr := resultFromCompletion(ev, agent.AgentInvocation{NodePath: "graph[0]"}, "")
	if eerr != nil {
		t.Fatalf("resultFromCompletion: %v", eerr)
	}
	if res.Output != nil {
		t.Errorf("Output = %v, want nil (no schema → no typed output; matches claude)", res.Output)
	}
	if res.Metrics.Tokens.Input != 10 || res.Metrics.Tokens.Output != 4 || res.Metrics.Turns != 2 {
		t.Errorf("metrics = %+v", res.Metrics)
	}
	if res.Metrics.Cost.Total != 0 {
		t.Errorf("Cost.Total = %v, want 0 (droid reports no cost)", res.Metrics.Cost.Total)
	}
}

func TestResultFromCompletion_SuccessWithSchema_ParsesJSON(t *testing.T) {
	// Prose then the JSON object; all one backtick raw string (\" and \n are JSON
	// escapes, not Go escapes).
	ev, _ := parseStreamEvent([]byte(`{"type":"completion","finalText":"Here is the answer: {\"answer\": 42}","numTurns":1,"durationMs":10}`))
	inv := agent.AgentInvocation{NodePath: "graph[0]", OutputSchema: &ir.JSONSchema{"type": "object"}}
	res, eerr := resultFromCompletion(ev, inv, "")
	if eerr != nil {
		t.Fatalf("resultFromCompletion: %v", eerr)
	}
	if v, ok := res.Output["answer"].(float64); !ok || v != 42 {
		t.Errorf("Output[answer] = %v (%T)", res.Output["answer"], res.Output["answer"])
	}
}

func TestResultFromCompletion_SchemaButFinalTextNotJSON_Unparseable(t *testing.T) {
	ev, _ := parseStreamEvent([]byte(`{"type":"completion","finalText":"no json","numTurns":1,"durationMs":10}`))
	_, eerr := resultFromCompletion(ev, agent.AgentInvocation{NodePath: "graph[2]", OutputSchema: &ir.JSONSchema{"type": "object"}}, "")
	var unp *agent.ErrUnparseableOutput
	if !errors.As(eerr, &unp) || unp.NodePath != "graph[2]" {
		t.Fatalf("err = %v, want *agent.ErrUnparseableOutput{NodePath:graph[2]}", eerr)
	}
}

// TestDroid_SurfacesModel (F35) asserts that the model reported in the
// "system"/init event is threaded through to Metrics.Model on the completion
// result — mirroring agent/claude's ExtractResult(msg, initModel). The
// pricing-table rate lookup for that model is DEFERRED (see the
// resultFromCompletion doc comment); this only asserts the id is surfaced.
func TestDroid_SurfacesModel(t *testing.T) {
	initEv, err := parseStreamEvent([]byte(`{"type":"system","model":"gpt-5-codex","tools":["read","write"]}`))
	if err != nil {
		t.Fatalf("parseStreamEvent(system): %v", err)
	}
	capturedModel := initEv.Model

	ev, err := parseStreamEvent([]byte(`{"type":"completion","finalText":"done","numTurns":1,"durationMs":10}`))
	if err != nil {
		t.Fatalf("parseStreamEvent(completion): %v", err)
	}
	res, eerr := resultFromCompletion(ev, agent.AgentInvocation{NodePath: "graph[0]"}, capturedModel)
	if eerr != nil {
		t.Fatalf("resultFromCompletion: %v", eerr)
	}
	if res.Metrics.Model != "gpt-5-codex" {
		t.Errorf("Metrics.Model = %q, want %q", res.Metrics.Model, "gpt-5-codex")
	}
}

func TestErrorFromEvent_Auth(t *testing.T) {
	ev, _ := parseStreamEvent([]byte(`{"type":"error","source":"cli","message":"Error: Authentication failed. Please log in using /login or set a valid FACTORY_API_KEY environment variable."}`))
	err := errorFromEvent(ev)
	if !errors.Is(err, ErrAuthFailureSentinel) {
		t.Fatalf("err = %v, want wrapped ErrAuthFailureSentinel (retryable in Launch)", err)
	}
}

func TestParseStreamEvent_BadJSON(t *testing.T) {
	_, err := parseStreamEvent([]byte(`not json`))
	var sp *ErrStreamParse
	if !errors.As(err, &sp) {
		t.Fatalf("err = %v, want *ErrStreamParse", err)
	}
}

func TestExtractJSONObject_StringAware(t *testing.T) {
	cases := []struct {
		name, in, wantKey string
		wantVal           string
	}{
		{"plain", `{"k":"v"}`, "k", "v"},
		{"prose-prefix", `here you go: {"k":"v"}`, "k", "v"},
		{"fenced", "```json\n{\"k\":\"v\"}\n```", "k", "v"},
		{"braces-in-string", `{"k":"has } and { inside"}`, "k", "has } and { inside"},
		{"escaped-quotes", `prefix {"k":"a \"quote\" and {nested}"} suffix`, "k", `a "quote" and {nested}`},
		{"multiple-last-wins", `{"k":"first"} then {"k":"second"}`, "k", "second"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := extractJSONObject(c.in)
			if err != nil {
				t.Fatalf("extractJSONObject(%q): %v", c.in, err)
			}
			if m[c.wantKey] != c.wantVal {
				t.Errorf("got %v[%q] = %v, want %q", m, c.wantKey, m[c.wantKey], c.wantVal)
			}
		})
	}
	if _, err := extractJSONObject("no object here"); err == nil {
		t.Error("extractJSONObject(no object): err = nil, want error")
	}
}
