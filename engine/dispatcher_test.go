package engine_test

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/engine"
)

func TestDispatchResultCarriesTranscript(t *testing.T) {
	ri := engine.ResolvedInputs{
		Thread: []agent.ThreadTurn{{User: "u1", Assistant: "a1"}},
	}
	if len(ri.Thread) != 1 || ri.Thread[0].User != "u1" || ri.Thread[0].Assistant != "a1" {
		t.Fatalf("ResolvedInputs.Thread = %+v, want one {u1,a1} turn", ri.Thread)
	}
	dr := engine.DispatchResult{
		Outcome:    engine.OutcomeOK,
		Transcript: agent.ThreadTurn{User: "uX", Assistant: "aX"},
	}
	if dr.Transcript.User != "uX" || dr.Transcript.Assistant != "aX" {
		t.Fatalf("DispatchResult.Transcript = %+v, want {uX,aX}", dr.Transcript)
	}
	// zero-value DispatchResult has zero Transcript
	var zero engine.DispatchResult
	if zero.Transcript != (agent.ThreadTurn{}) {
		t.Fatalf("zero DispatchResult.Transcript = %+v, want zero", zero.Transcript)
	}
}
