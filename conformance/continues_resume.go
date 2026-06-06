package conformance

import (
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	agentfake "github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
)

// testContinuesResume is T3 from the spec (Phase 4 Task 4.7): prove that RESUME
// mid-conversation works correctly — committed turns are NOT re-launched, and
// the frontier turn's Thread is rebuilt purely from the FOLDED committed
// transcripts (not from a still-live adapter).
//
// The workflow has three agent steps in a linear continues: chain:
//
//	t1 ← t2 (continues: t1) ← t3 (continues: t2)
//
// The test crashes exactly at t3's node.completed Append via FailAppendAfterN(6):
//
//	k=0: run.started
//	k=1: node.started t1   (observational; error discarded)
//	k=2: node.completed t1 ← t1 committed
//	k=3: node.started t2   (observational; error discarded)
//	k=4: node.completed t2 ← t2 committed
//	k=5: node.started t3   (observational; error discarded)
//	k=6: node.completed t3 ← crash target (t3 uncommitted)
//
// Assertions (the three T3 claims):
//
//	(a) the resume fake's Calls contain NO entry for t1/t2 — committed turns
//	    are folded (replayed from log), not re-launched.
//	(b) t3's recorded inv.Thread equals the two committed verbatim pairs
//	    [{t1.user,t1.assistant},{t2.user,t2.assistant}] in root→current order —
//	    proving Fold materialized NodeResult.Transcript and assembly rebuilt the
//	    thread from the log, not from a live adapter.
//	(c) pre-crash vs post-resume NodeResult for t1/t2 are reflect.DeepEqual
//	    (incl. the Transcript field), proving fold fidelity across the resume boundary.
//
// This test is a regression guard: it is GREEN immediately (since Tasks 4.3–4.5
// already implement the correct behavior). If it were RED, that would indicate
// a real resume bug that must be fixed, NOT a test to be weakened.
func testContinuesResume(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("fold_reconstructs_thread_on_resume", func(t *testing.T) {
		testFoldReconstructsThreadOnResume(t, factory)
	})
}

// threeTurnContinuesWorkflow is a 3-step linear continues: chain served by
// the awf/llm containerless adapter. t2 continues t1; t3 continues t2.
// All three steps declare an output_schema (typed outputs required by the engine).
// No containers: block (Containerless=true adapter).
const threeTurnContinuesWorkflow = `workflow: conformance-continues-resume
version: 1
graph:
  - id: t1
    uses: awf/llm
    with:
      model: m
      prompt: "turn one"
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties:
        k: { type: string }
  - id: t2
    uses: awf/llm
    continues: t1
    with:
      model: m
      prompt: "turn two"
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties:
        k: { type: string }
  - id: t3
    uses: awf/llm
    continues: t2
    with:
      model: m
      prompt: "turn three"
    output_schema:
      type: object
      additionalProperties: false
      required: [k]
      properties:
        k: { type: string }
`

func testFoldReconstructsThreadOnResume(t *testing.T, factory BackendFactory) {
	t.Helper()

	// First-run fake: scripts t1 (index 0) and t2 (index 1) with distinct
	// verbatim transcript pairs. t3 (index 2) is NOT scripted — the run crashes
	// before t3's node.completed lands so the fake is never asked for index 2.
	firstFake := agentfake.New("awf/llm").
		WithCaps(agent.Caps{Containerless: true, Threaded: true}).
		Script(0, agentfake.Result{
			Output:     map[string]any{"k": "v1"},
			Transcript: agent.ThreadTurn{User: "p1", Assistant: "a1"},
		}).
		Script(1, agentfake.Result{
			Output:     map[string]any{"k": "v2"},
			Transcript: agent.ThreadTurn{User: "p2", Assistant: "a2"},
		}).
		// Script t3 at index 2 to satisfy the Launch call before the induced
		// crash. FailAppendAfterN(6) halts AFTER node.completed t2 and on
		// node.completed t3's Append — but t3 does run through Launch and
		// Commit's Blobs.Put before the crashing Append. Without a script at 2,
		// the fake errors on the Launch itself (before the crashing Append) and
		// the test would fail at the wrong point.
		Script(2, agentfake.Result{
			Output:     map[string]any{"k": "v3"},
			Transcript: agent.ThreadTurn{User: "p3", Assistant: "a3"},
		})

	register := func(reg *agent.Registry) {
		if err := reg.Register(firstFake); err != nil {
			t.Fatalf("Register firstFake: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, threeTurnContinuesWorkflow, register)

	// Crash at t3's node.completed Append (k=6). The six preceding Appends
	// (run.started, node.started t1, node.completed t1, node.started t2,
	// node.completed t2, node.started t3) all succeed — so t1 and t2 are fully
	// committed (transcript blobs + node.completed) while t3 is uncommitted.
	h.log.FailAppendAfterN(6)

	_, firstErr := h.runWorkflow(t)
	if firstErr == nil {
		t.Fatal("FailAppendAfterN(6): err = nil, want induced-fault error; t3 fault did not fire")
	}

	// Snapshot the committed prefix (t1 + t2) via Fold.
	preEvents := mustFoldEvents(t, h)
	preRS, err := engine.Fold(preEvents, h.blobs)
	if err != nil {
		t.Fatalf("Fold pre-resume: %v", err)
	}
	snap := make(map[string]engine.NodeResult, 2)
	for _, id := range []string{"t1", "t2"} {
		nr, ok := preRS.Completed[id]
		if !ok {
			t.Fatalf("pre-resume Fold missing committed %q — crash did not leave the expected committed prefix", id)
		}
		snap[id] = nr
	}

	// Resume fake: program ONLY t3 at index 0 (fresh fake — index counter
	// starts from 0). If the interpreter re-dispatches t1 or t2, their calls
	// consume index 0 and 1, causing t3's script at index 0 to collide and
	// the test to either fail at the wrong step or pass vacuously; we catch
	// this via the len(calls) == 1 assertion below.
	resumeFake := agentfake.New("awf/llm").
		WithCaps(agent.Caps{Containerless: true, Threaded: true}).
		Script(0, agentfake.Result{
			Output:     map[string]any{"k": "v3"},
			Transcript: agent.ThreadTurn{User: "p3", Assistant: "a3"},
		})

	// Swap the harness's agent registry for the resume's fresh fake.
	// The dispatcher reads h.agentRegistry at runOrResume call time, so
	// replacing the pointer here is sufficient.
	h.agentRegistry = &agent.Registry{}
	if err := h.agentRegistry.Register(resumeFake); err != nil {
		t.Fatalf("Register resumeFake: %v", err)
	}
	h.log.ClearFault()
	h.blobs.ClearFault()

	resumeOutcome, resumeErr := h.resumeWorkflow(t)
	if resumeErr != nil {
		t.Fatalf("resume: %v", resumeErr)
	}
	if resumeOutcome != engine.OutcomeOK {
		t.Fatalf("resume outcome = %v, want ok", resumeOutcome)
	}

	calls := resumeFake.Calls()

	// (a) Exactly ONE launch on resume — t3 only. t1 and t2 must replay from
	// the log (CLAUDE.md "Resume folds the log: committed steps are replayed,
	// not recomputed").
	if len(calls) != 1 {
		t.Fatalf("resume fake Calls = %d, want 1 (t1/t2 must replay, not re-launch); calls=%+v",
			len(calls), calls)
	}

	// (b) t3's Thread must equal the two committed verbatim pairs in
	// root→current order: [{p1,a1}, {p2,a2}]. This proves Fold materialized
	// NodeResult.Transcript from the log and assembly rebuilt the thread from
	// the folded committed state (not from a still-live adapter).
	got := calls[0].Thread
	want := []agent.ThreadTurn{
		{User: "p1", Assistant: "a1"},
		{User: "p2", Assistant: "a2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("t3 Thread = %+v, want %+v", got, want)
	}

	// (c) Pre/post committed-prefix NodeResult.Transcript are byte-identical
	// across the resume boundary — proving fold fidelity.
	postEvents := mustFoldEvents(t, h)
	postRS, err := engine.Fold(postEvents, h.blobs)
	if err != nil {
		t.Fatalf("Fold post-resume: %v", err)
	}
	for _, id := range []string{"t1", "t2"} {
		post, ok := postRS.Completed[id]
		if !ok {
			t.Errorf("post-resume Completed missing %q", id)
			continue
		}
		if !reflect.DeepEqual(snap[id].Transcript, post.Transcript) {
			t.Errorf("%s Transcript drifted across resume: pre=%+v post=%+v",
				id, snap[id].Transcript, post.Transcript)
		}
	}
}
