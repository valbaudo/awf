package droid

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

func TestExtractResult_SuccessNoSchema_NilOutput(t *testing.T) {
	env, err := parseEnvelope([]byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":"all done","usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":1,"cache_creation_input_tokens":2}}`))
	if err != nil {
		t.Fatalf("parseEnvelope: %v", err)
	}
	res, eerr := extractResult(env, agent.AgentInvocation{NodePath: "graph[0]"})
	if eerr != nil {
		t.Fatalf("extractResult: %v", eerr)
	}
	if res.Output != nil {
		t.Errorf("Output = %v, want nil (no schema → no typed output; matches claude)", res.Output)
	}
	if res.Metrics.Tokens.Input != 10 || res.Metrics.Tokens.Output != 4 || res.Metrics.Turns != 2 {
		t.Errorf("metrics = %+v", res.Metrics)
	}
	if res.Metrics.Cost.USD != 0 {
		t.Errorf("Cost.USD = %v, want 0 (droid reports no cost)", res.Metrics.Cost.USD)
	}
}

func TestExtractResult_SuccessWithSchema_ParsesJSON(t *testing.T) {
	// Prose then the JSON object; all one backtick raw string (\" and \n are JSON
	// escapes, not Go escapes).
	env, _ := parseEnvelope([]byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"Here is the answer: {\"answer\": 42}"}`))
	inv := agent.AgentInvocation{NodePath: "graph[0]", OutputSchema: &ir.JSONSchema{"type": "object"}}
	res, eerr := extractResult(env, inv)
	if eerr != nil {
		t.Fatalf("extractResult: %v", eerr)
	}
	if v, ok := res.Output["answer"].(float64); !ok || v != 42 {
		t.Errorf("Output[answer] = %v (%T)", res.Output["answer"], res.Output["answer"])
	}
}

func TestExtractResult_SchemaButResultNotJSON_Unparseable(t *testing.T) {
	env, _ := parseEnvelope([]byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"sorry, no json here"}`))
	_, eerr := extractResult(env, agent.AgentInvocation{NodePath: "graph[2]", OutputSchema: &ir.JSONSchema{"type": "object"}})
	var unp *agent.ErrUnparseableOutput
	if !errors.As(eerr, &unp) || unp.NodePath != "graph[2]" {
		t.Fatalf("err = %v, want *agent.ErrUnparseableOutput{NodePath:graph[2]}", eerr)
	}
}

func TestExtractResult_AuthFailure_Retryable(t *testing.T) {
	env, _ := parseEnvelope([]byte(`{"type":"result","subtype":"failure","is_error":true,"num_turns":0,"result":"Authentication failed. ... set a valid FACTORY_API_KEY environment variable."}`))
	_, eerr := extractResult(env, agent.AgentInvocation{NodePath: "graph[0]"})
	if !errors.Is(eerr, ErrAuthFailureSentinel) {
		t.Fatalf("err = %v, want wrapped ErrAuthFailureSentinel (retryable in Launch)", eerr)
	}
}

func TestParseEnvelope_BadJSON(t *testing.T) {
	_, err := parseEnvelope([]byte(`not json`))
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
