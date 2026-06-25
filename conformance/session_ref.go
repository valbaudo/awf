package conformance

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	agentfake "github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// testSessionRef is the Bucket-19 entry point: session transcript capture →
// fold → restore on the generator frontier.
//
// M1 scope: the full conformance harness e2e (run → capture SessionRef in
// node.completed → resume → WriteFileAt restore) is DEFERRED to Milestone 2.
// Reason: the harness drives engine.Run which calls runAgentStepWithContext;
// that function sets ResolvedInputs.SessionTranscriptPath only when the agent
// adapter signals Caps.PersistentSession (the M2 adapter wiring). Without an
// M2 PersistentSession adapter, SessionTranscriptPath is always empty and the
// capture/restore blocks are never entered via YAML-driven runs.
//
// M1 ships a FAKE-ONLY engine-level sub-test (below) that proves:
//   - A fold-committed SessionRef in RunState seeds restore on the next run.
//   - The restore calls Backend.WriteFileAt with the committed transcript bytes.
//   - The fake's WriteFileAtCalls recorder captures it.
//   - A run with no committed SessionRef produces 0 WriteFileAt calls.
//
// The full e2e conformance harness test is registered as a skip here; it will
// be promoted to a live assertion when the M2 session adapter is implemented.
func testSessionRef(t *testing.T, factory BackendFactory) {
	t.Helper()
	if _, ok := factory().(*container.Fake); !ok {
		t.Skip("session_ref asserts on the fake's WriteFileAtCalls recorder; fake-only")
	}
	t.Run("restore_calls_write_file_at", func(t *testing.T) { testSessionRefRestoreCallsWriteFileAt(t) })
	t.Run("no_restore_on_first_run", func(t *testing.T) { testSessionRefNoRestoreOnFirstRun(t) })
	t.Run("session_ref_blob_round_trip", func(t *testing.T) { testSessionRefBlobRoundTrip(t) })
	t.Run("full_e2e_conformance_harness", func(t *testing.T) {
		// Deferred to M2: the YAML-driven path (runAgentStepWithContext) only
		// sets ResolvedInputs.SessionTranscriptPath for PersistentSession adapters,
		// which are wired in Milestone 2. Without that adapter wiring, the
		// capture/restore blocks are unreachable from engine.Run with a real
		// workflow YAML.
		t.Skip("deferred to M2: requires a PersistentSession adapter to set ResolvedInputs.SessionTranscriptPath via runAgentStepWithContext")
	})
}

// testSessionRefRestoreCallsWriteFileAt verifies the restore site: when RunState
// carries a SessionRef for the dispatched node path and ResolvedInputs has a
// SessionTranscriptPath, the dispatcher calls Backend.WriteFileAt with the
// committed blob bytes before launching the agent.
func testSessionRefRestoreCallsWriteFileAt(t *testing.T) {
	t.Helper()
	blobs := state.NewInMemoryBlobs()
	f := container.NewFake().WithBlobs(blobs)
	h, err := f.Create(t.Context(), container.ContainerSpec{Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Seed a transcript blob and a SessionRef for node path "gen".
	transcript := []byte(`{"session":"round1","history":[]}`)
	ref, putErr := blobs.Put(transcript)
	if putErr != nil {
		t.Fatalf("blobs.Put: %v", putErr)
	}
	rs := engine.NewRunState("run-1", "digest-1", nil)
	rs.SessionRefs["gen"] = ref

	// Dispatch the agent with RunState + Blobs set on the dispatcher.
	const transcriptPath = "/home/agent/.claude/projects/p/s.jsonl"
	dr := dispatchOKAgentSessionForConformance(t, f, h, rs, blobs, engine.ResolvedInputs{
		SessionTranscriptPath: transcriptPath,
	})
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q (Err: %v)", dr.Outcome, engine.OutcomeOK, dr.Err)
	}

	// WriteFileAt must have been called with the committed transcript bytes.
	var found bool
	for _, c := range f.WriteFileAtCalls {
		if c.Path == transcriptPath && string(c.Content) == string(transcript) {
			found = true
		}
	}
	if !found {
		t.Errorf("restore did not call WriteFileAt with the committed transcript (WriteFileAtCalls=%+v)", f.WriteFileAtCalls)
	}

	// The transcript blob survives in the shared store (content-addressed durability).
	got, getErr := blobs.Get(ref)
	if getErr != nil {
		t.Errorf("transcript blob not durable after restore: %v", getErr)
	}
	if string(got) != string(transcript) {
		t.Errorf("blob round-trip = %q, want %q", got, transcript)
	}
}

// testSessionRefNoRestoreOnFirstRun verifies that when RunState has no
// SessionRefs entry for the dispatched node path, WriteFileAt is NOT called
// (first run with no prior session).
func testSessionRefNoRestoreOnFirstRun(t *testing.T) {
	t.Helper()
	blobs := state.NewInMemoryBlobs()
	f := container.NewFake().WithBlobs(blobs)
	h, err := f.Create(t.Context(), container.ContainerSpec{Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rs := engine.NewRunState("run-1", "digest-1", nil) // no SessionRefs["gen"]

	// Pre-write the transcript file so the capture block (ReadFileAt) succeeds
	// and the step returns OutcomeOK. WriteFileAtCalls is then reset so the seed
	// write is not counted — WriteFileAt is only called by the restore path.
	const transcriptPath = "/home/agent/.claude/projects/p/s.jsonl"
	if wErr := f.WriteFileAt(t.Context(), h, transcriptPath, []byte("transcript")); wErr != nil {
		t.Fatalf("seed transcript: %v", wErr)
	}
	f.WriteFileAtCalls = nil

	dr := dispatchOKAgentSessionForConformance(t, f, h, rs, blobs, engine.ResolvedInputs{
		SessionTranscriptPath: transcriptPath,
	})
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q (Err: %v)", dr.Outcome, engine.OutcomeOK, dr.Err)
	}
	if len(f.WriteFileAtCalls) != 0 {
		t.Errorf("WriteFileAt called on first run (no prior session), want 0 calls; got %+v", f.WriteFileAtCalls)
	}
}

// testSessionRefBlobRoundTrip verifies the full Commit + Fold path: a
// DispatchResult with a SessionTranscript is content-addressed by Commit,
// recorded as SessionRef in NodeCompletedData, and Fold populates
// RunState.SessionRefs[path] — the prerequisite for restore on the next run.
func testSessionRefBlobRoundTrip(t *testing.T) {
	t.Helper()
	blobs := state.NewInMemoryBlobs()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)

	// engine.Fold requires run.started as the first event.
	if err := log.Append(state.Event{
		Type: engine.EventRunStarted,
		Data: []byte(`{"run_id":"r","workflow_digest":"d"}`),
	}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}

	transcript := []byte(`{"session":"round2"}`)
	dr := engine.DispatchResult{Outcome: engine.OutcomeOK, SessionTranscript: transcript}

	if _, commitErr := engine.Commit(log, blobs, "gen", dr, false); commitErr != nil {
		t.Fatalf("Commit: %v", commitErr)
	}

	// Fold must populate SessionRefs["gen"] from the node.completed event.
	events, foldErr := log.Fold()
	if foldErr != nil {
		t.Fatalf("log.Fold: %v", foldErr)
	}
	rs, rsErr := engine.Fold(events, blobs)
	if rsErr != nil {
		t.Fatalf("engine.Fold: %v", rsErr)
	}
	ref, ok := rs.SessionRefs["gen"]
	if !ok || ref == "" {
		t.Fatalf("SessionRefs[gen] not populated after Fold (rs.SessionRefs=%v)", rs.SessionRefs)
	}

	// The blob survives and matches the original transcript.
	got, getErr := blobs.Get(ref)
	if getErr != nil {
		t.Fatalf("blobs.Get(SessionRef): %v", getErr)
	}
	if string(got) != string(transcript) {
		t.Errorf("blob = %q, want %q", got, transcript)
	}

	// The node.completed event carries the session_ref field.
	var foundRef string
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted && e.Path == "gen" {
			var d engine.NodeCompletedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal NodeCompletedData: %v", err)
			}
			foundRef = d.SessionRef
		}
	}
	if foundRef != ref {
		t.Errorf("NodeCompletedData.SessionRef = %q, want %q", foundRef, ref)
	}
}

// dispatchOKAgentSessionForConformance is the conformance-local dispatcher
// helper: builds a LocalDispatcher backed by f with RunState + Blobs set,
// registers a scripted fake agent, dispatches through d.Run, and returns the
// DispatchResult. The container is registered as "ws".
func dispatchOKAgentSessionForConformance(t *testing.T, f *container.Fake, h container.Handle, rs *engine.RunState, blobs *state.InMemoryBlobs, ri engine.ResolvedInputs) engine.DispatchResult {
	t.Helper()
	fk := agentfake.New("test/session-agent").Script(0, agentfake.Result{Output: map[string]any{"ok": true}})
	reg := &agent.Registry{}
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ri.Uses = "test/session-agent"
	d := &engine.LocalDispatcher{
		Backend:  f,
		Handles:  map[string]container.Handle{"ws": h},
		Resolver: reg,
		RunState: rs,
		Blobs:    blobs,
	}
	intent := engine.NodeIntent{
		Path:           "gen",
		Node:           &ir.AgentStep{ID: "gen", Container: "ws", Uses: "test/session-agent"},
		ResolvedInputs: ri,
	}
	dr, ch, err := d.Run(t.Context(), intent)
	for range ch {
	}
	if err != nil {
		t.Fatalf("dispatchOKAgentSessionForConformance: Run engine-level error: %v", err)
	}
	return dr
}
