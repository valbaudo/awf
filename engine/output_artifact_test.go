package engine_test

// TestRunAgentStep_OutputArtifactSerializesTypedOutput proves that a
// containerless agent step with output_artifact: result has the dispatcher
// serialize its validated typed Output as canonical JSON into Files["result"],
// which Commit then content-addresses — so rs.LookupCompleted("draft").Files
// carries a CAS ref whose bytes are byte-identical to json.Marshal(Output).
//
// This is Task B4. The byte-identity assertion covers two invariants:
//  1. The artifact content equals marshalCanonicalJSON(Output).
//  2. The OutputsRef blob and Files["result"] blob have the SAME content
//     (and therefore the same CAS ref), because both go through
//     marshalCanonicalJSON — idempotent Put per spec §8.

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

func TestRunAgentStep_OutputArtifactSerializesTypedOutput(t *testing.T) {
	// Containerless one-step workflow: draft, uses awf/llm, output_artifact: result.
	// IR struct avoids YAML loading overhead and makes the test self-contained.
	wf := &ir.Workflow{
		ID:      "output-artifact-typed",
		Version: 1,
		Graph: ir.NodeList{
			&ir.AgentStep{
				ID:             "draft",
				Uses:           "awf/llm",
				With:           ir.RawConfig{"model": "m", "prompt": "draft it"},
				OutputSchema:   &ir.JSONSchema{"type": "object"},
				OutputArtifact: "result",
				// Container is intentionally empty (containerless step).
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	var reg agent.Registry
	fk := fake.New("awf/llm").
		WithCaps(agent.Caps{Containerless: true}).
		Script(0, fake.Result{Output: map[string]any{"label": "x", "score": 2}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	be := container.NewFake()
	dispatcher := &engine.LocalDispatcher{Backend: be, Handles: map[string]container.Handle{}, Resolver: &reg}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	nr, ok := rs.LookupCompleted("draft")
	if !ok {
		t.Fatalf("RunState.Completed missing 'draft'")
	}

	// (a) The "result" artifact must appear in the committed Files map.
	cas, ok := nr.Files["result"]
	if !ok {
		t.Fatalf("committed Files = %+v, want a 'result' entry", nr.Files)
	}

	// (b) Retrieve the blob and assert byte-identity with the canonical marshal
	// of the Output (encoding/json sorts map keys lexically → deterministic).
	got, err := blobs.Get(cas)
	if err != nil {
		t.Fatalf("blobs.Get(%q): %v", cas, err)
	}
	want, err := json.Marshal(map[string]any{"label": "x", "score": 2})
	if err != nil {
		t.Fatalf("json.Marshal want: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("artifact content = %q, want canonical %q", got, want)
	}

	// (c) OutputsRef and Files["result"] are byte-identical, so their CAS refs
	// must be equal (idempotent Put: same bytes → same ref).
	if nr.OutputsRef != cas {
		t.Fatalf("OutputsRef = %q, Files[result] ref = %q; want equal (byte-identical marshal → same CAS ref)", nr.OutputsRef, cas)
	}
}

// TestRunAgentStep_OutputArtifactContainerBackedSerializesTypedOutput is the
// F39 counterpart of the test above: output_artifact is no longer
// containerless-only, so a CONTAINER-backed agent step (Container: "lab")
// must get the same treatment — the dispatcher serializes its validated
// typed Output into Files["result"] (local_dispatcher_agent.go's bare==""
// guard dropped).
func TestRunAgentStep_OutputArtifactContainerBackedSerializesTypedOutput(t *testing.T) {
	wf := &ir.Workflow{
		ID:      "output-artifact-container-backed",
		Version: 1,
		Graph: ir.NodeList{
			&ir.AgentStep{
				ID:             "draft",
				Container:      "lab",
				Uses:           "anthropic/claude-code",
				With:           ir.RawConfig{"prompt": "draft it"},
				OutputSchema:   &ir.JSONSchema{"type": "object"},
				OutputArtifact: "result",
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").
		Script(0, fake.Result{Output: map[string]any{"label": "x", "score": 2}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	be := container.NewFake()
	h, err := be.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab handle: %v", err)
	}
	dispatcher := &engine.LocalDispatcher{Backend: be, Handles: map[string]container.Handle{"lab": h}, Resolver: &reg}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1c", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1c", "d", nil)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	nr, ok := rs.LookupCompleted("draft")
	if !ok {
		t.Fatalf("RunState.Completed missing 'draft'")
	}

	cas, ok := nr.Files["result"]
	if !ok {
		t.Fatalf("committed Files = %+v, want a 'result' entry", nr.Files)
	}

	got, err := blobs.Get(cas)
	if err != nil {
		t.Fatalf("blobs.Get(%q): %v", cas, err)
	}
	want, err := json.Marshal(map[string]any{"label": "x", "score": 2})
	if err != nil {
		t.Fatalf("json.Marshal want: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("artifact content = %q, want canonical %q", got, want)
	}

	if nr.OutputsRef != cas {
		t.Fatalf("OutputsRef = %q, Files[result] ref = %q; want equal (byte-identical marshal → same CAS ref)", nr.OutputsRef, cas)
	}
}
