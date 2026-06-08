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
		// Threaded:true — continues: requires a Threaded adapter; the engine
		// guard (defense-in-depth) will reject a non-Threaded adapter when
		// inv.Thread is non-empty. The real claude-code adapter sets Threaded:true.
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true}).
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

// TestRunAgentStep_AssemblesThreadRootToCurrent pins the 3-turn root→current
// ordering invariant (Task 5.2). A linear chain:
//
//	t1 (no continues) → t2 continues:t1 → t3 continues:t2
//
// All three share the same adapter so a single fake handles them in call
// order 0→1→2. After a clean engine.Run:
//   - calls[0].Thread (t1) is empty (no predecessor).
//   - calls[1].Thread (t2) is exactly [{u1,a1}] — t1's committed pair only.
//   - calls[2].Thread (t3) is exactly [{u1,a1},{u2,a2}] — t1 BEFORE t2
//     (root→current, oldest first).
//
// This test PINS assembly order — if the order is reversed or a turn is
// missing, that is a real assembly bug; do not weaken the assertion.
func TestRunAgentStep_AssemblesThreadRootToCurrent(t *testing.T) {
	const yaml = `workflow: continues-3turn
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: t1
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "p1"
    output_schema:
      type: object
      additionalProperties: false
      required: [x]
      properties:
        x: { type: string }
  - id: t2
    container: lab
    uses: anthropic/claude-code
    continues: t1
    with:
      prompt: "p2"
    output_schema:
      type: object
      additionalProperties: false
      required: [x]
      properties:
        x: { type: string }
  - id: t3
    container: lab
    uses: anthropic/claude-code
    continues: t2
    with:
      prompt: "p3"
    output_schema:
      type: object
      additionalProperties: false
      required: [x]
      properties:
        x: { type: string }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").
		// Threaded:true — continues: requires a Threaded adapter; the engine
		// guard (defense-in-depth) will reject a non-Threaded adapter when
		// inv.Thread is non-empty. The real claude-code adapter sets Threaded:true.
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true}).
		Script(0, fake.Result{
			Output:     map[string]any{"x": "v1"},
			Transcript: agent.ThreadTurn{User: "u1", Assistant: "a1"},
		}).
		Script(1, fake.Result{
			Output:     map[string]any{"x": "v2"},
			Transcript: agent.ThreadTurn{User: "u2", Assistant: "a2"},
		}).
		Script(2, fake.Result{
			Output:     map[string]any{"x": "v3"},
			Transcript: agent.ThreadTurn{User: "u3", Assistant: "a3"},
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
	if len(calls) != 3 {
		t.Fatalf("len(calls) = %d, want 3", len(calls))
	}

	// t1: no continues → Thread must be empty.
	if len(calls[0].Thread) != 0 {
		t.Errorf("t1 Thread = %+v, want empty (no predecessor)", calls[0].Thread)
	}

	// t2: continues:t1 → Thread is exactly [t1 pair] (single entry).
	want2 := []agent.ThreadTurn{{User: "u1", Assistant: "a1"}}
	if len(calls[1].Thread) != 1 {
		t.Fatalf("t2 Thread len = %d, want 1; got %+v", len(calls[1].Thread), calls[1].Thread)
	}
	if calls[1].Thread[0] != want2[0] {
		t.Errorf("t2 Thread[0] = %+v, want %+v", calls[1].Thread[0], want2[0])
	}

	// t3: continues:t2 → Thread is [t1 pair, t2 pair] in ROOT→CURRENT order.
	// t1 must be Thread[0] (oldest/root) and t2 must be Thread[1] (immediate predecessor).
	// Do NOT reverse this assertion: root→current is the invariant.
	want3 := []agent.ThreadTurn{
		{User: "u1", Assistant: "a1"}, // t1 — root, oldest
		{User: "u2", Assistant: "a2"}, // t2 — immediate predecessor
	}
	if len(calls[2].Thread) != 2 {
		t.Fatalf("t3 Thread len = %d, want 2; got %+v", len(calls[2].Thread), calls[2].Thread)
	}
	if calls[2].Thread[0] != want3[0] {
		t.Errorf("t3 Thread[0] = %+v, want %+v (root turn t1 must be FIRST)", calls[2].Thread[0], want3[0])
	}
	if calls[2].Thread[1] != want3[1] {
		t.Errorf("t3 Thread[1] = %+v, want %+v (predecessor t2 must be SECOND)", calls[2].Thread[1], want3[1])
	}
}

// TestRunAgentStep_LeafTranscriptNotCommitted pins the participates invariant:
// only a turn that is continued-FROM (a "thread target") commits a transcript
// blob. A leaf turn — one that continues someone else but nobody continues IT —
// must NOT commit a transcript (its NodeCompletedData.TranscriptRef is empty).
//
// Chain: turn1 (target of turn2) → turn2 (leaf, continues turn1, no successor).
// Expected: turn1.TranscriptRef != "" ; turn2.TranscriptRef == "".
func TestRunAgentStep_LeafTranscriptNotCommitted(t *testing.T) {
	const yaml = `workflow: leaf-no-transcript
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
		WithCaps(agent.Caps{NativeSchema: true, Threaded: true}).
		Script(0, fake.Result{
			Output:     map[string]any{"k": "v1"},
			Transcript: agent.ThreadTurn{User: "u1", Assistant: "a1"},
		}).
		Script(1, fake.Result{
			Output:     map[string]any{"k": "v2"},
			Transcript: agent.ThreadTurn{User: "u2", Assistant: "a2"},
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

	// Scan the committed node.completed events and check TranscriptRef presence.
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	transcriptRef := map[string]string{} // step path → TranscriptRef (empty means not committed)
	for _, ev := range events {
		if ev.Type != engine.EventNodeCompleted {
			continue
		}
		var d engine.NodeCompletedData
		if uerr := json.Unmarshal(ev.Data, &d); uerr != nil {
			t.Fatalf("unmarshal NodeCompletedData at %q: %v", ev.Path, uerr)
		}
		transcriptRef[ev.Path] = d.TranscriptRef
	}

	// turn1 is a thread target (turn2 continues it) → its transcript MUST be committed.
	if transcriptRef["turn1"] == "" {
		t.Errorf("turn1 (thread target): TranscriptRef is empty, want non-empty")
	}
	// turn2 is a leaf (no step continues it) → its transcript must NOT be committed.
	if transcriptRef["turn2"] != "" {
		t.Errorf("turn2 (leaf): TranscriptRef = %q, want empty (leaf turns must not commit transcript blobs)", transcriptRef["turn2"])
	}
}

// TestRunAgentStep_ContainerlessInputFilesRejected pins the SP1 containerless
// guard (Task 7b): an agent step whose runtime omits container: (Container == "")
// declaring input_files is rejected at runtime as a permanent_failure, exactly
// as the man page promises ("input_files requires a container ... rejected on a
// containerless agent step"). The guard fires in the interpreter BEFORE
// resolution, so no producer needs to have committed.
func TestRunAgentStep_ContainerlessInputFilesRejected(t *testing.T) {
	var reg agent.Registry
	// A containerless adapter — the only kind permitted to carry an empty
	// container: at run start (cli/runtimes.go). The fake's Containerless cap
	// lets the dispatcher's own guard pass; we are pinning the interpreter guard.
	fk := fake.New("awf/llm").
		WithCaps(agent.Caps{NativeSchema: true, Containerless: true}).
		Script(0, fake.Result{Output: map[string]any{"k": "v"}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Graph: ir.NodeList{
			&ir.AgentStep{
				ID:         "hunt",
				Uses:       "awf/llm",
				With:       ir.RawConfig{"prompt": "go"},
				InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"},
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	dispatcher := &engine.LocalDispatcher{Backend: container.NewFake(), Resolver: &reg}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, io.Discard, nil)
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (containerless input_files must be rejected)", oc, engine.OutcomePermanentFailure)
	}
	if err == nil || !strings.Contains(err.Error(), "input_files requires a container") {
		t.Errorf("err = %v, want one mentioning 'input_files requires a container'", err)
	}
	// The adapter must never be launched — the guard short-circuits before dispatch.
	if len(fk.Calls()) != 0 {
		t.Errorf("fake.Calls len = %d, want 0 (guard must fire before Launch)", len(fk.Calls()))
	}
}

// TestRunAgentStep_StagesInputFiles pins the SP1 agent-step staging wiring
// (Task 7b): a CODE producer (certified named output_files) hands an artifact to
// a containerized AGENT consumer via input_files. The interpreter resolves the
// committed CAS ref, Blobs.Get's the bytes, and the dispatcher CopyTo's them into
// the agent's container BEFORE Launch. Mirrors the code-step cross-container test.
func TestRunAgentStep_StagesInputFiles(t *testing.T) {
	be := container.NewFake()
	labH, err := be.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab: %v", err)
	}
	boxH, err := be.Create(context.Background(), container.ContainerSpec{Name: "box"})
	if err != nil {
		t.Fatalf("Create box: %v", err)
	}

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{Output: map[string]any{"k": "v"}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The code producer's exec produces /out/report.md (seeded onto lab's handle).
	sentinel := []byte("recon findings\n")
	be.ProgramExec("./recon.sh", container.ExecResult{ExitCode: 0}, nil)
	if err := be.WriteFile(labH, "/out/report.md", sentinel); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	disp := &engine.LocalDispatcher{
		Backend:  be,
		Handles:  map[string]container.Handle{"lab": labH, "box": boxH},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", nil)

	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{"lab": {}, "box": {}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "recon", Container: "lab", Run: "./recon.sh",
				OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/report.md"}},
			},
			&ir.AgentStep{
				ID: "hunt", Container: "box", Uses: "anthropic/claude-code",
				With:         ir.RawConfig{"prompt": "go"},
				OutputSchema: &ir.JSONSchema{"type": "object", "additionalProperties": false, "required": []any{"k"}, "properties": map[string]any{"k": map[string]any{"type": "string"}}},
				InputFiles:   map[string]string{"/work/report.md": "step.recon.files.report"},
			},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, io.Discard, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	if _, ok := rs.LookupCompleted("hunt"); !ok {
		t.Fatal("RunState.Completed missing 'hunt'")
	}
	// The sentinel was seeded ONLY into lab; hunt runs in box. The staged bytes
	// landing in box at /work/report.md proves resolve→Get→CopyTo-before-Launch.
	got, err := be.CaptureFiles(context.Background(), boxH, []string{"/work/report.md"})
	if err != nil {
		t.Fatalf("CaptureFiles box /work/report.md: %v", err)
	}
	if len(got) != 1 || string(got[0].Content) != string(sentinel) {
		t.Errorf("staged into box = %+v, want one file with content %q", got, sentinel)
	}
}

func TestRunAgentStep_OutputFilesPathTemplatedAndCommitted(t *testing.T) {
	const yaml = `workflow: agent-output-files
version: 1
input:
  type: object
  additionalProperties: false
  required: [cve_id]
  properties:
    cve_id: { type: string }
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: write_record
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "write the record"
    output_schema:
      type: object
      additionalProperties: false
      required: [ok]
      properties:
        ok: { type: boolean }
    output_files:
      record: "/work/records/{{ input.cve_id }}.json"
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"ok": true},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	be := container.NewFake()
	h, err := be.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab: %v", err)
	}
	if err := be.WriteFile(h, "/work/records/CVE-2026-0001.json", []byte(`{"cve":"CVE-2026-0001"}`)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dispatcher := &engine.LocalDispatcher{
		Backend:  be,
		Handles:  map[string]container.Handle{"lab": h},
		Resolver: &reg,
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	rs := engine.NewRunState("r1", "d", map[string]any{"cve_id": "CVE-2026-0001"})

	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, io.Discard, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	nr, ok := rs.LookupCompleted("write_record")
	if !ok {
		t.Fatal("RunState.Completed missing write_record")
	}
	cas, ok := nr.Files["/work/records/CVE-2026-0001.json"]
	if !ok {
		t.Fatalf("Committed files = %+v, want substituted record path", nr.Files)
	}
	got, err := blobs.Get(cas)
	if err != nil {
		t.Fatalf("Blobs.Get: %v", err)
	}
	if string(got) != `{"cve":"CVE-2026-0001"}` {
		t.Errorf("Blob content = %q", got)
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
