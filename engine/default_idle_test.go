package engine_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// coarseIdleDefaultYAML is a single container-backed agent step that leaves
// timeout: unset — the D3 per-tier default watchdog must fill it in the
// runtime-only ResolvedInputs based on the adapter's measured Coarse liveness.
const coarseIdleDefaultYAML = `workflow: coarse-idle-default
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: gen
    container: lab
    uses: test/coarse
    with:
      prompt: "p"
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok: { type: boolean }
`

// TestRunAgentStep_DefaultIdle_CoarseTier: a Coarse-tier adapter with no author
// idle: gets the per-tier default filled in ResolvedInputs — idle 90s + a 30s
// startup grace — without the workflow ever declaring a timeout. Captured off the
// dispatched NodeIntent (the interpreter's deliverable) via capturingDispatcher.
func TestRunAgentStep_DefaultIdle_CoarseTier(t *testing.T) {
	ld := loadAgentSimpleDef(t, coarseIdleDefaultYAML)

	var reg agent.Registry
	fk := fake.New("test/coarse").
		WithCaps(agent.Caps{NativeSchema: true, SurfacesLiveness: agent.LivenessCoarse}).
		Script(0, fake.Result{Output: map[string]any{"ok": true}})
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
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), ld, rs, cap, log, state.NewInMemoryBlobs(), clk, engine.RunOptions{Tap: io.Discard})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q", oc)
	}

	if len(cap.captured) != 1 {
		t.Fatalf("captured intents = %d, want 1", len(cap.captured))
	}
	ri := cap.captured[0].ResolvedInputs
	if ri.IdleTimeout != 90*time.Second {
		t.Errorf("ResolvedInputs.IdleTimeout = %v, want 90s (Coarse-tier default)", ri.IdleTimeout)
	}
	if ri.StartupGrace != 30*time.Second {
		t.Errorf("ResolvedInputs.StartupGrace = %v, want 30s (Coarse-tier default)", ri.StartupGrace)
	}
}

// TestRunAgentStep_DefaultIdle_NoneTierLeavesUnset: an unmeasured (None) adapter
// gets no idle default — wall-clock only — so a workflow that never opted into a
// stall watchdog is not silently given one.
func TestRunAgentStep_DefaultIdle_NoneTierLeavesUnset(t *testing.T) {
	const yaml = `workflow: none-idle-default
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: gen
    container: lab
    uses: test/blind
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
	fk := fake.New("test/blind").
		WithCaps(agent.Caps{NativeSchema: true, SurfacesLiveness: agent.LivenessNone}).
		Script(0, fake.Result{Output: map[string]any{"ok": true}})
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
	rs := engine.NewRunState("r1", "d", nil)

	if _, err := engine.Run(context.Background(), ld, rs, cap, log, state.NewInMemoryBlobs(), clk, engine.RunOptions{Tap: io.Discard}); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if len(cap.captured) != 1 {
		t.Fatalf("captured intents = %d, want 1", len(cap.captured))
	}
	ri := cap.captured[0].ResolvedInputs
	if ri.IdleTimeout != 0 {
		t.Errorf("ResolvedInputs.IdleTimeout = %v, want 0 (None tier — wall-clock only)", ri.IdleTimeout)
	}
	if ri.StartupGrace != 0 {
		t.Errorf("ResolvedInputs.StartupGrace = %v, want 0 (None tier)", ri.StartupGrace)
	}
}

// TestRunAgentStep_DefaultIdle_DigestUnchanged (test c): applying the per-tier
// idle default must NOT mutate the workflow IR. If a future change materialized
// the default into ir.AgentStep.Timeout, ComputeDigest would drift and trip the
// resume drift hard-error — this locks the "runtime-only" invariant.
func TestRunAgentStep_DefaultIdle_DigestUnchanged(t *testing.T) {
	ld := loadAgentSimpleDef(t, coarseIdleDefaultYAML)

	before, err := ld.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest (before): %v", err)
	}

	var reg agent.Registry
	fk := fake.New("test/coarse").
		WithCaps(agent.Caps{NativeSchema: true, SurfacesLiveness: agent.LivenessCoarse}).
		Script(0, fake.Result{Output: map[string]any{"ok": true}})
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
	rs := engine.NewRunState("r1", "d", nil)

	if _, err := engine.Run(context.Background(), ld, rs, dispatcher, log, state.NewInMemoryBlobs(), clk, engine.RunOptions{Tap: io.Discard}); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	after, err := ld.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest (after): %v", err)
	}
	if before != after {
		t.Fatalf("ComputeDigest drifted after default-idle application:\n before = %s\n after  = %s\n(the per-tier default must never be written into ir.AgentStep.Timeout)", before, after)
	}
}
