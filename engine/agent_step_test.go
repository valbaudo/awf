package engine_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

// loadAgentSimpleDef is a test helper — writes the YAML to a temp file, loads
// it, and returns the LoadedDefinition. (Mirrors patterns from engine/interpreter_test.go.)
func loadAgentSimpleDef(t *testing.T, yaml string) *ir.LoadedDefinition {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/wf.yaml"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	ld, err := loader.Load(path)
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		t.Fatalf("ir.Validate: %v", diags)
	}
	return ld
}

func TestRunAgentStep_HappyPath_TemplatedWith(t *testing.T) {
	const yaml = `workflow: agent-step-templated
version: 1
input:
  type: object
  required: [topic]
  additionalProperties: false
  properties:
    topic: { type: string }
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: triage
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "summarize {{ input.topic }}"
    output_schema:
      type: object
      additionalProperties: false
      required: [verdict]
      properties:
        verdict: { type: string }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"verdict": "approved"},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	be := container.NewFake()
	handles := map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}}
	dispatcher := &engine.LocalDispatcher{Backend: be, Handles: handles, Resolver: &reg}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", map[string]any{"topic": "frogs"})

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, io.Discard, nil)
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(calls))
	}
	if got := calls[0].With["prompt"]; got != "summarize frogs" {
		t.Errorf("With[prompt] = %v, want %q (template not substituted)", got, "summarize frogs")
	}

	nr, ok := rs.LookupCompleted("triage")
	if !ok {
		t.Fatalf("Completed[triage] missing")
	}
	if nr.Outputs["verdict"] != "approved" {
		t.Errorf("Outputs[verdict] = %v, want %q", nr.Outputs["verdict"], "approved")
	}
}

func TestRunAgentStep_AgentEventLogEntries(t *testing.T) {
	const yaml = `workflow: agent-events
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: triage
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "p"
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok: { type: boolean }
`
	ld := loadAgentSimpleDef(t, yaml)
	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"ok": true},
		Events: []agent.AgentEvent{
			{Kind: "system", Payload: []byte(`{"subtype":"init"}`)},
			{Kind: "result", Payload: []byte(`{"subtype":"success"}`)},
		},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, io.Discard, nil)
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q", oc)
	}

	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	count := 0
	for _, ev := range events {
		if ev.Type == engine.EventAgentEvent && ev.Path == "triage" {
			count++
			var data engine.AgentEventData
			if uerr := json.Unmarshal(ev.Data, &data); uerr != nil {
				t.Fatalf("unmarshal agent.event Data: %v", uerr)
			}
			if data.Kind == "" {
				t.Errorf("AgentEventData.Kind empty in %+v", data)
			}
		}
	}
	if count != 2 {
		t.Errorf("agent.event log entry count = %d, want 2", count)
	}
}

func TestRunAgentStep_ThreadAssembledFromContinues(t *testing.T) {
	// Two sequential agent steps; turn2 continues: turn1. Both use the same
	// uses: so AWF1028 (same-runtime) passes. The fake scripts a distinct
	// verbatim transcript pair per call. After the run, turn2's invocation
	// must carry turn1's committed pair as its single Thread entry, and
	// turn1's invocation must have an empty Thread.
	const yaml = `workflow: continues-linear
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: turn1
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "p1"
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties:
        k: { type: string }
  - id: turn2
    container: lab
    uses: anthropic/claude-code
    continues: turn1
    with:
      prompt: "p2"
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties:
        k: { type: string }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").
		Script(0, fake.Result{
			Output:     map[string]any{"k": "v1"},
			Transcript: agent.ThreadTurn{User: "prompt-1", Assistant: "answer-1"},
		}).
		Script(1, fake.Result{
			Output:     map[string]any{"k": "v2"},
			Transcript: agent.ThreadTurn{User: "prompt-2", Assistant: "answer-2"},
		})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, io.Discard, nil)
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2", len(calls))
	}
	// turn1: no continues -> empty Thread.
	if len(calls[0].Thread) != 0 {
		t.Errorf("turn1 Thread = %+v, want empty", calls[0].Thread)
	}
	// turn2: Thread is the single committed turn1 pair (root->current order).
	if len(calls[1].Thread) != 1 {
		t.Fatalf("turn2 Thread len = %d, want 1", len(calls[1].Thread))
	}
	if calls[1].Thread[0].User != "prompt-1" || calls[1].Thread[0].Assistant != "answer-1" {
		t.Errorf("turn2 Thread[0] = %+v, want {prompt-1,answer-1}", calls[1].Thread[0])
	}
}

// mustJSON is a per-package test helper. Task 2 defined an identical body
// in engine/events_test.go (package engine — internal); this file is
// package engine_test (external), a separate scope, so we declare it
// again here. Standard Go idiom for crossing the internal/external test
// boundary (cf. stdlib net/http's export_test.go vs http_test.go).
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// capturingDispatcher wraps a base Dispatcher and records each NodeIntent
// it receives — the test inspects intent.ResolvedInputs.Feedback to verify
// runAgentStep's gate-repair feedback population (Task 9). Task 10 verifies
// the AgentInvocation.Feedback end-to-end via the fake adapter; this test
// scopes to runAgentStep's deliverable in isolation so it can be bisected
// to Task 9 without depending on Task 10's threading change in runAgent.
type capturingDispatcher struct {
	inner    engine.Dispatcher
	captured []engine.NodeIntent
}

func (c *capturingDispatcher) Run(ctx context.Context, intent engine.NodeIntent) (engine.DispatchResult, <-chan container.IOChunk, error) {
	c.captured = append(c.captured, intent)
	return c.inner.Run(ctx, intent)
}

func TestRunAgentStep_FeedbackPopulatedOnGateRepair(t *testing.T) {
	const yaml = `workflow: gate-agent-feedback
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - gate:
      max_attempts: 2
      until: "{{ evaluate.verified }}"
      generate:
        - id: gen
          container: lab
          uses: anthropic/claude-code
          with:
            prompt: "write the thing"
          output_schema:
            type: object
            additionalProperties: false
            required: [done]
            properties:
              done: { type: boolean }
      evaluate:
        - id: judge
          container: lab
          uses: anthropic/claude-code
          with:
            prompt: "evaluate"
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, feedback]
            properties:
              verified: { type: boolean }
              feedback: { type: string }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	// Indices 0 (gen attempt 1) and 2 (gen attempt 2) — generator outputs.
	// Indices 1 (judge attempt 1) and 3 (judge attempt 2) — evaluator verdicts.
	// Same adapter ref handles both; fake's call index advances per Launch.
	fk := fake.New("anthropic/claude-code").
		Script(0, fake.Result{Output: map[string]any{"done": false}}).
		Script(1, fake.Result{Output: map[string]any{"verified": false, "feedback": "missing detection"}}).
		Script(2, fake.Result{Output: map[string]any{"done": true}}).
		Script(3, fake.Result{Output: map[string]any{"verified": true, "feedback": ""}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	base := &engine.LocalDispatcher{
		Backend:  container.NewFake(),
		Handles:  map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}},
		Resolver: &reg,
	}
	cap := &capturingDispatcher{inner: base}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, cap, log, blobs, clk, io.Discard, nil)
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q", oc)
	}

	if len(cap.captured) != 4 {
		t.Fatalf("captured intents = %d, want 4 (2 generate + 2 evaluate)", len(cap.captured))
	}

	// Locate the generate-attempt-2 intent — its path contains
	// ".attempt-2.generate". That's the one runAgentStep must populate with
	// the prior verdict from attempt 1's evaluator.
	var gen2 *engine.NodeIntent
	var gen1 *engine.NodeIntent
	for i := range cap.captured {
		path := cap.captured[i].Path
		if !strings.Contains(path, ".generate") {
			continue
		}
		if strings.Contains(path, ".attempt-1.") {
			gen1 = &cap.captured[i]
		}
		if strings.Contains(path, ".attempt-2.") {
			gen2 = &cap.captured[i]
		}
	}
	if gen1 == nil {
		t.Fatal("generate attempt-1 intent not captured")
	}
	if gen2 == nil {
		t.Fatal("generate attempt-2 intent not captured")
	}

	// Attempt 1: no prior verdict → Feedback must be nil.
	if gen1.ResolvedInputs.Feedback != nil {
		t.Errorf("gen1.ResolvedInputs.Feedback = %v, want nil (attempt 1, no prior verdict)", gen1.ResolvedInputs.Feedback)
	}

	// Attempt 2: Feedback must carry attempt-1's evaluator verdict.
	if gen2.ResolvedInputs.Feedback == nil {
		t.Fatalf("gen2.ResolvedInputs.Feedback = nil, want populated with prior verdict")
	}
	if gen2.ResolvedInputs.Feedback["verified"] != false {
		t.Errorf("gen2.ResolvedInputs.Feedback[verified] = %v, want false", gen2.ResolvedInputs.Feedback["verified"])
	}
	if gen2.ResolvedInputs.Feedback["feedback"] != "missing detection" {
		t.Errorf("gen2.ResolvedInputs.Feedback[feedback] = %v, want %q", gen2.ResolvedInputs.Feedback["feedback"], "missing detection")
	}
}
