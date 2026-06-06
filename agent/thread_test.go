package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestThreadTurn_JSONTags(t *testing.T) {
	tt := ThreadTurn{User: "u1", Assistant: "a1"}
	b, err := json.Marshal(tt)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"user":"u1"`) {
		t.Errorf("ThreadTurn JSON %q missing `\"user\":\"u1\"`", got)
	}
	if !strings.Contains(got, `"assistant":"a1"`) {
		t.Errorf("ThreadTurn JSON %q missing `\"assistant\":\"a1\"`", got)
	}

	var rt ThreadTurn
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if rt != tt {
		t.Errorf("round-trip = %+v, want %+v", rt, tt)
	}
}

func TestAgentInvocation_ThreadField(t *testing.T) {
	// Distinct strings on BOTH halves of each turn — otherwise thread
	// assertions in later phases are vacuous (spec B.2).
	inv := AgentInvocation{
		Uses: "awf/llm",
		Thread: []ThreadTurn{
			{User: "u1", Assistant: "a1"},
			{User: "u2", Assistant: "a2"},
		},
	}
	if len(inv.Thread) != 2 {
		t.Fatalf("len(Thread) = %d, want 2", len(inv.Thread))
	}
	if inv.Thread[0].User == inv.Thread[0].Assistant {
		t.Errorf("turn 0 halves are equal (%q); distinct strings required", inv.Thread[0].User)
	}
	if inv.Thread[1].User != "u2" || inv.Thread[1].Assistant != "a2" {
		t.Errorf("turn 1 = %+v, want {u2 a2}", inv.Thread[1])
	}
}

func TestAgentInvocation_ThreadOmitEmpty(t *testing.T) {
	b, err := json.Marshal(AgentInvocation{Uses: "awf/llm"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "thread") {
		t.Errorf("nil Thread serialized %q, want it omitted (omitempty)", b)
	}
}
