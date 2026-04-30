package engine_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
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
