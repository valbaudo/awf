package engine_test

// TestOutputArtifact_InGateAndForwardOut (Task B5, positive) proves the full
// in-gate file channel:
//
//  1. A containerless awf/llm generate step with output_artifact:result commits
//     its typed Output as a CAS blob in Files["result"].
//  2. A container-backed evaluator reads that artifact via input_files (the
//     engine stages it into the container before the adapter runs).
//  3. The gate passes on attempt-1 (evaluate.ok == true).
//  4. The workflow output_files.final forward-out resolves to the accepted
//     attempt's artifact blob (passedGateArtifactRuntimePath wiring).
//
// TestContainerlessOutputFilesStillRejected (negative) proves that a
// containerless step declaring output_files (not output_artifact) still gets
// OutcomePermanentFailure with "output_files requires a container".

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// gateOutputArtifactWF is the YAML fixture (also saved as
// examples/awf-llm-gate-extract/workflow.yaml). Notes vs. the task brief:
//   - until: "{{ evaluate.ok }}" — the correct gate pattern; the brief's
//     "{{ judge.ok }}" resolves as an `as`-binding and errors at runtime
//     because `judge` is an agent step, not a react step and there is no
//     enclosing map with as:judge.
//   - max_attempts: 3 — required; the gate executor errors on MaxAttempts < 1.
const gateOutputArtifactWF = `workflow: awf-llm-gate-extract
version: 1
input:
  type: object
  additionalProperties: false
  properties: {}
containers:
  scorer:
    image: oci://example.com/scorer@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - gate:
      max_attempts: 3
      generate:
        - id: draft
          uses: awf/llm
          with:
            provider: anthropic
            model: claude-sonnet-4-6
            max_tokens: 1024
            prompt: "Extract key points."
          output_schema:
            type: object
            additionalProperties: false
            required: [punti]
            properties:
              punti:
                type: array
                items: { type: string }
          output_artifact: result
      evaluate:
        - id: judge
          container: scorer
          uses: example/scorer
          input_files:
            /in/draft.json: step.draft.files.result
          with:
            prompt: "score"
          output_schema:
            type: object
            additionalProperties: false
            required: [ok]
            properties:
              ok: { type: boolean }
      until: "{{ evaluate.ok }}"
output_files:
  final: step.draft.files.result
`

// containerlessWithOutputFilesWF is the negative fixture: a containerless
// awf/llm step that declares output_files (not output_artifact). The
// validator accepts this (it does not know at static time that awf/llm is
// containerless), but the runtime dispatcher rejects it with
// OutcomePermanentFailure and "output_files requires a container".
const containerlessWithOutputFilesWF = `workflow: containerless-output-files
version: 1
graph:
  - id: draft
    uses: awf/llm
    with:
      provider: anthropic
      model: claude-sonnet-4-6
      max_tokens: 256
      prompt: "Produce a JSON file."
    output_schema:
      type: object
      additionalProperties: false
      required: [x]
      properties:
        x: { type: string }
    output_files:
      x: /out/x.json
`

func TestOutputArtifact_InGateAndForwardOut(t *testing.T) {
	ld := loadAgentSimpleDef(t, gateOutputArtifactWF)

	var reg agent.Registry
	// Containerless generate step: awf/llm returns {punti: ["a"]}.
	if err := reg.Register(fake.New("awf/llm").
		WithCaps(agent.Caps{Containerless: true}).
		Script(0, fake.Result{Output: map[string]any{"punti": []any{"a"}}})); err != nil {
		t.Fatalf("Register awf/llm: %v", err)
	}
	// Container-backed evaluator: example/scorer returns {ok: true} → gate passes on attempt-1.
	if err := reg.Register(fake.New("example/scorer").
		Script(0, fake.Result{Output: map[string]any{"ok": true}})); err != nil {
		t.Fatalf("Register example/scorer: %v", err)
	}

	be := container.NewFake()
	h, err := be.Create(context.Background(), container.ContainerSpec{Name: "scorer"})
	if err != nil {
		t.Fatalf("Create scorer handle: %v", err)
	}
	d := &engine.LocalDispatcher{
		Backend:  be,
		Handles:  map[string]container.Handle{"scorer": h},
		Resolver: &reg,
	}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg := state.NewInMemoryLog(clk)
	if err := lg.Append(state.Event{
		Type: engine.EventRunStarted,
		Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"}),
	}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", map[string]any{})

	oc, runErr := engine.Run(context.Background(), ld, rs, d, lg, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if runErr != nil || oc != engine.OutcomeOK {
		t.Fatalf("engine.Run: oc=%v err=%v", oc, runErr)
	}

	// Verify the generate step's NodeResult has the artifact committed.
	nr, ok := rs.LookupCompleted("gate[0].attempt-1.generate.draft")
	if !ok {
		t.Fatalf("generate step not committed at gate[0].attempt-1.generate.draft")
	}
	if _, ok := nr.Files["result"]; !ok {
		t.Fatalf("draft step Files has no 'result' entry; Files = %+v", nr.Files)
	}

	// forward-out: EvaluateExports resolves output_files.final to the accepted
	// attempt's artifact via passedGateArtifactRuntimePath.
	res, err := engine.EvaluateExports(rs, ld.Workflow, "", map[string]any{}, blobs)
	if err != nil {
		t.Fatalf("EvaluateExports: %v", err)
	}
	ref, ok := res.Files["final"]
	if !ok {
		t.Fatalf("EvaluateExports.Files has no 'final' key; got %+v", res.Files)
	}

	got, err := blobs.Get(ref)
	if err != nil {
		t.Fatalf("blobs.Get(%q): %v", ref, err)
	}
	want, err := json.Marshal(map[string]any{"punti": []any{"a"}})
	if err != nil {
		t.Fatalf("json.Marshal want: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("forwarded artifact content = %q, want canonical %q", got, want)
	}
}

func TestContainerlessOutputFilesStillRejected(t *testing.T) {
	ld := loadAgentSimpleDef(t, containerlessWithOutputFilesWF)

	var reg agent.Registry
	// Register awf/llm as containerless — it will never be reached because
	// the dispatcher rejects output_files before launching the adapter.
	if err := reg.Register(fake.New("awf/llm").
		WithCaps(agent.Caps{Containerless: true}).
		Script(0, fake.Result{Output: map[string]any{"x": "ignored"}})); err != nil {
		t.Fatalf("Register: %v", err)
	}

	be := container.NewFake()
	d := &engine.LocalDispatcher{
		Backend:  be,
		Handles:  map[string]container.Handle{},
		Resolver: &reg,
	}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg := state.NewInMemoryLog(clk)
	if err := lg.Append(state.Event{
		Type: engine.EventRunStarted,
		Data: mustJSON(engine.RunStartedData{RunID: "r2", WorkflowDigest: "d2"}),
	}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r2", "d2", nil)

	oc, runErr := engine.Run(context.Background(), ld, rs, d, lg, blobs, clk, engine.RunOptions{Tap: io.Discard})
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (containerless output_files must be rejected)", oc, engine.OutcomePermanentFailure)
	}
	// engine.Run propagates the dispatcher's error through failStep all the way
	// to the caller: (OutcomePermanentFailure, cause). The failed step is NOT
	// committed to RunState (fold ignores node.failed events), so there is no
	// LookupCompleted entry to inspect. Check runErr directly.
	if runErr == nil || !strings.Contains(runErr.Error(), "output_files requires a container") {
		t.Fatalf("err = %v, want it to contain %q", runErr, "output_files requires a container")
	}
}
