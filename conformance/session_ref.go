package conformance

import (
	"context"
	"encoding/json"
	"fmt"
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

// sessionConformanceFake is a test-only agent adapter used by the full e2e
// conformance test. It wraps *agentfake.Fake and additionally implements
// agent.SessionPathProvider (returning the fixed path sessionConformancePath).
// Caps.PersistentSession is set to true so the M2 interpreter wiring sets
// ResolvedInputs.SessionTranscriptPath for steps using this adapter.
type sessionConformanceFake struct {
	*agentfake.Fake
}

const sessionConformancePath = "/transcript.jsonl"

func (s *sessionConformanceFake) SessionTranscriptPath(_ agent.AgentInvocation, _ string) string {
	return sessionConformancePath
}

// Compile-time assertion: sessionConformanceFake satisfies agent.SessionPathProvider.
var _ agent.SessionPathProvider = (*sessionConformanceFake)(nil)

// sessionE2EWorkflow is the workflow YAML used by testSessionRefFullE2E.
// It has two steps:
//   - gen: a PersistentSession agent step (uses "test/session-e2e-agent",
//     retry:{attempts:1}) that captures a SessionRef on success.
//   - downstream: a code step (retry:{attempts:1}) that is crashed by
//     FailExecAfterN to halt the frontier after gen commits.
//
// retry:{attempts:1} on every step pins the one-shot FailExecAfterN fault so
// it actually halts the step instead of being recovered by retries (mirrors
// the snapshot conformance test's snapshotWorkspaceWorkflow).
var sessionE2EWorkflow = fmt.Sprintf(`workflow: conformance-session-e2e
version: 1
containers:
  ws:
    image: %[1]s
graph:
  - id: gen
    container: ws
    uses: test/session-e2e-agent
    with:
      prompt: "hello"
    retry: { attempts: 1 }
  - id: downstream
    container: ws
    run: "./downstream.sh"
    retry: { attempts: 1 }
`, fakeImageDigest)

// testSessionRef is the Bucket-19 entry point: session transcript capture →
// fold → restore on the generator frontier.
//
// Sub-tests:
//   - restore_calls_write_file_at: dispatcher-level restore (M1, already green).
//   - no_restore_on_first_run: no WriteFileAt on a first run (M1, already green).
//   - session_ref_blob_round_trip: Commit+Fold round-trip (M1, already green).
//   - full_e2e_conformance_harness: YAML-driven capture + fold + restore (M2, promoted
//     from skip now that the interpreter wires SessionTranscriptPath).
func testSessionRef(t *testing.T, factory BackendFactory) {
	t.Helper()
	if _, ok := factory().(*container.Fake); !ok {
		t.Skip("session_ref asserts on the fake's WriteFileAtCalls recorder; fake-only")
	}
	t.Run("restore_calls_write_file_at", func(t *testing.T) { testSessionRefRestoreCallsWriteFileAt(t) })
	t.Run("no_restore_on_first_run", func(t *testing.T) { testSessionRefNoRestoreOnFirstRun(t) })
	t.Run("session_ref_blob_round_trip", func(t *testing.T) { testSessionRefBlobRoundTrip(t) })
	t.Run("full_e2e_conformance_harness", func(t *testing.T) { testSessionRefFullE2E(t) })
}

// testSessionRefFullE2E is the M2 full-workflow e2e (promoted from M1-deferred skip).
// It exercises the complete session-ref pipeline through the real harness:
//
//  1. Run 1 (YAML-driven via runWorkflow):
//     - gen (PersistentSession, sessionConformanceFake) runs, its transcript is
//     present in the fake container at sessionConformancePath → ReadFileAt captures
//     it → node.completed carries session_ref → RunState.SessionRefs["gen"] populated.
//     - downstream (code step) is crashed by FailExecAfterN → downstream is the frontier.
//
//  2. Fold the log → assert RunState.SessionRefs["gen"] is set (interpreter wiring proof).
//
//  3. Resume (harness resumeWorkflow): downstream re-runs and succeeds.
//     The resume completes with OutcomeOK.
//
//  4. Restore-path proof: after folding the completed log, dispatch gen again (as if
//     it were on the frontier) using a fresh fake backed by the SAME shared blobs.
//     The folded RunState carries SessionRefs["gen"]; the dispatcher must call
//     Backend.WriteFileAt with the committed transcript bytes before launching.
//     This proves the full capture → fold → restore pipeline end-to-end.
//
// Structure mirrors testSnapshotRestoreCalledOnResume (conformance/snapshot.go):
// shared InMemoryBlobs across run/resume/proof, FailExecAfterN to halt the frontier,
// run-fake / resume-fake recorded for assertions.
func testSessionRefFullE2E(t *testing.T) {
	t.Helper()
	blobs := state.NewInMemoryBlobs()

	// Session adapter: PersistentSession + SessionPathProvider → fixed transcript path.
	sessionAdapter := &sessionConformanceFake{
		Fake: agentfake.New("test/session-e2e-agent").
			WithCaps(agent.Caps{NativeSchema: true, PersistentSession: true}).
			Script(0, agentfake.Result{}). // run 1: gen succeeds
			Script(1, agentfake.Result{}), // proof dispatch: gen succeeds again
	}

	h := newHarnessWithAgentRegistry(t,
		func() container.Backend { return container.NewFake().WithBlobs(blobs) },
		sessionE2EWorkflow,
		func(reg *agent.Registry) {
			if err := reg.Register(sessionAdapter); err != nil {
				t.Fatalf("Register session adapter: %v", err)
			}
		},
	)
	h.blobs = blobs

	// Track run/resume fakes for assertions.
	// NOTE: this e2e uses a fixed-path fake adapter (sessionConformanceFake.SessionTranscriptPath
	// returns sessionConformancePath unconditionally and ignores workdir). It does NOT exercise
	// the real workdir-based path derivation (encodeProjectDir) used by the live claude-code-session
	// adapter. The workdir gap is tracked by TODO(M2d) in engine/agent_step.go.
	var runFake, resumeFake *container.Fake
	h.factory = func() container.Backend {
		f := container.NewFake().WithBlobs(blobs)
		f.ProgramExec("./downstream.sh", container.ExecResult{ExitCode: 0}, nil)
		if runFake == nil {
			f.FailExecAfterN(0) // crash the first Exec (downstream.sh on run 1)
			runFake = f
		} else {
			resumeFake = f
		}
		return &sessionSeedingFake{Fake: f, transcriptPath: sessionConformancePath, transcript: []byte(`{"session":"e2e-run1"}`)}
	}

	// First run: gen commits (SessionRef captured), downstream crashes → frontier.
	outcome1, _ := h.runWorkflow(t)
	if outcome1 == "" {
		t.Fatal("first run produced no outcome (harness error before workflow evaluated)")
	}
	if outcome1 == engine.OutcomeOK {
		t.Fatal("first run unexpectedly returned ok — FailExecAfterN(0) did not crash downstream")
	}

	// Assert: gen's node.completed carries session_ref.
	events1 := mustFoldEvents(t, h)
	var genSessionRef string
	for _, ev := range events1 {
		if ev.Type == engine.EventNodeCompleted && ev.Path == "gen" {
			var d engine.NodeCompletedData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatalf("unmarshal gen node.completed: %v", err)
			}
			genSessionRef = d.SessionRef
		}
	}
	if genSessionRef == "" {
		t.Fatal("gen node.completed has no session_ref — capture did not fire (interpreter wiring not working?)")
	}

	// Fold: RunState.SessionRefs["gen"] must be populated from the committed event.
	foldedRS, ferr := engine.Fold(events1, blobs)
	if ferr != nil {
		t.Fatalf("Fold after run 1: %v", ferr)
	}
	if foldedRS.SessionRefs["gen"] == "" {
		t.Fatalf("RunState.SessionRefs[gen] empty after Fold — fold did not carry session_ref into RunState (SessionRefs=%v)", foldedRS.SessionRefs)
	}
	if foldedRS.SessionRefs["gen"] != genSessionRef {
		t.Fatalf("RunState.SessionRefs[gen] = %q, want %q (fold ref mismatch)", foldedRS.SessionRefs["gen"], genSessionRef)
	}

	// Resume: downstream re-runs and succeeds → run completes OK.
	outcome2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Fatalf("resume: %v", err2)
	}
	if outcome2 != engine.OutcomeOK {
		t.Fatalf("resume outcome = %q, want ok", outcome2)
	}
	if resumeFake == nil {
		t.Fatal("resume did not mint a second fake")
	}

	// The transcript blob must survive in shared blobs after run + resume.
	transcriptBytes, getErr := blobs.Get(genSessionRef)
	if getErr != nil {
		t.Fatalf("transcript blob not durable after resume: %v", getErr)
	}

	// Restore-path proof (mirrors the snapshot test's "Restore was called" assertion):
	// Dispatch gen again using the fully-folded RunState (after both run + resume).
	// The folded RunState still has SessionRefs["gen"] from run 1's commit (downstream's
	// completion on resume does not clear gen's SessionRef).
	// The fresh fake must call WriteFileAt at sessionConformancePath with the committed bytes.
	finalEvents := mustFoldEvents(t, h)
	finalRS, fferr := engine.Fold(finalEvents, blobs)
	if fferr != nil {
		t.Fatalf("Fold after resume: %v", fferr)
	}
	if finalRS.SessionRefs["gen"] == "" {
		t.Fatalf("RunState.SessionRefs[gen] empty after resume Fold — session_ref should survive (it was not invalidated)")
	}

	// Build a fresh fake (as if gen were re-dispatched on a new run with prior session).
	proofFake := container.NewFake().WithBlobs(blobs)
	ph, phErr := proofFake.Create(t.Context(), container.ContainerSpec{Name: "ws"})
	if phErr != nil {
		t.Fatalf("Create proof fake: %v", phErr)
	}
	// Pre-seed the transcript for the capture side of the proof dispatch.
	if wErr := proofFake.WriteFileAt(t.Context(), ph, sessionConformancePath, transcriptBytes); wErr != nil {
		t.Fatalf("seed transcript in proof fake: %v", wErr)
	}
	proofFake.WriteFileAtCalls = nil // reset so seed write is not counted

	// Register the session adapter for the proof dispatch (Script(1) = gen's second call).
	proofReg := &agent.Registry{}
	if regErr := proofReg.Register(sessionAdapter); regErr != nil {
		t.Fatalf("Register session adapter for proof: %v", regErr)
	}

	proofRI := engine.ResolvedInputs{
		Uses:                  "test/session-e2e-agent",
		SessionTranscriptPath: sessionConformancePath,
	}
	proofD := &engine.LocalDispatcher{
		Backend:  proofFake,
		Handles:  map[string]container.Handle{"ws": ph},
		Resolver: proofReg,
		RunState: finalRS,
		Blobs:    blobs,
	}
	proofIntent := engine.NodeIntent{
		Path:           "gen",
		Node:           &ir.AgentStep{ID: "gen", Container: "ws", Uses: "test/session-e2e-agent"},
		ResolvedInputs: proofRI,
	}
	proofDR, proofCh, proofErr := proofD.Run(t.Context(), proofIntent)
	for range proofCh {
	}
	if proofErr != nil {
		t.Fatalf("proof dispatch error: %v", proofErr)
	}
	if proofDR.Outcome != engine.OutcomeOK {
		t.Fatalf("proof dispatch outcome = %q, want ok (Err: %v)", proofDR.Outcome, proofDR.Err)
	}

	// WriteFileAt must have been called with the original committed transcript bytes.
	var restored bool
	for _, c := range proofFake.WriteFileAtCalls {
		if c.Path == sessionConformancePath && string(c.Content) == string(transcriptBytes) {
			restored = true
		}
	}
	if !restored {
		t.Errorf("restore-path proof: WriteFileAt not called at %q with committed transcript (WriteFileAtCalls=%+v)", sessionConformancePath, proofFake.WriteFileAtCalls)
	}
}

// sessionSeedingFake wraps *container.Fake and overrides Create to pre-seed a
// transcript file at transcriptPath in every handle it creates. This lets the
// conformance e2e pre-seed the agent transcript so Backend.ReadFileAt (the
// capture block) succeeds without requiring a separate pre-seeding step after
// the harness creates its handles internally.
type sessionSeedingFake struct {
	*container.Fake
	transcriptPath string
	transcript     []byte
}

func (s *sessionSeedingFake) Create(ctx context.Context, spec container.ContainerSpec) (container.Handle, error) {
	h, err := s.Fake.Create(ctx, spec)
	if err != nil {
		return h, err
	}
	// Pre-seed the transcript file in the newly created handle so that
	// Backend.ReadFileAt (the capture block) succeeds when gen runs.
	if wErr := s.WriteFileAt(ctx, h, s.transcriptPath, s.transcript); wErr != nil {
		return h, fmt.Errorf("sessionSeedingFake.Create: seed transcript: %w", wErr)
	}
	// Reset WriteFileAtCalls so the seed write is not visible to test assertions.
	s.WriteFileAtCalls = nil
	return h, nil
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
