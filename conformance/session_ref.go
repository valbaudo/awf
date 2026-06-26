package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// testSessionRef is the Bucket-19 entry point: session-subtree capture →
// fold → restore on the generator frontier.
//
// Sub-tests:
//   - restore_calls_write_tree_at: dispatcher-level restore (subtree model).
//   - no_restore_on_first_run: no WriteTreeAt on a first run.
//   - session_ref_blob_round_trip: Commit+Fold round-trip.
//   - full_e2e_conformance_harness: YAML-driven capture + fold + restore.
func testSessionRef(t *testing.T, factory BackendFactory) {
	t.Helper()
	if _, ok := factory().(*container.Fake); !ok {
		t.Skip("session_ref asserts on the fake's WriteTreeAtCalls recorder; fake-only")
	}
	t.Run("restore_calls_write_tree_at", func(t *testing.T) { testSessionRefRestoreCallsWriteTreeAt(t) })
	t.Run("no_restore_on_first_run", func(t *testing.T) { testSessionRefNoRestoreOnFirstRun(t) })
	t.Run("session_ref_blob_round_trip", func(t *testing.T) { testSessionRefBlobRoundTrip(t) })
	t.Run("full_e2e_conformance_harness", func(t *testing.T) { testSessionRefFullE2E(t) })
}

// sessionSeedingFake wraps *container.Fake and overrides ReadTreeAt to seed a
// transcript file UNDER the captured SessionDir on the first (empty) read —
// simulating claude having written $CLAUDE_CONFIG_DIR/projects/<bucket>/<uuid>.jsonl
// during the run. This lets the capture block's ReadTreeAt(SessionDir) succeed
// without the engine (or the test) knowing the RunID-derived SessionDir a priori.
// On the restore path the subtree is already populated (WriteTreeAt ran first),
// so the seed is a no-op. WriteTreeAt is recorded by the embedded *container.Fake's
// WriteTreeAtCalls (promoted method), so restore is observable via the underlying fake.
type sessionSeedingFake struct {
	*container.Fake
	transcript []byte
}

// ReadTreeAt seeds a transcript under dir on the first (empty) read, then
// delegates to the embedded fake (the capture path).
func (s *sessionSeedingFake) ReadTreeAt(ctx context.Context, h container.Handle, dir string) ([]byte, error) {
	if existing, _ := s.Fake.ReadTreeAt(ctx, h, dir); existing == nil {
		seedPath := strings.TrimRight(dir, "/") + "/-work/session.jsonl"
		if wErr := s.WriteFileAt(ctx, h, seedPath, s.transcript); wErr != nil {
			return nil, wErr
		}
	}
	return s.Fake.ReadTreeAt(ctx, h, dir)
}

// testSessionRefFullE2E is the full-workflow e2e for the subtree session model.
// It exercises the complete session-ref pipeline through the real harness:
//
//  1. Run 1 (YAML-driven via runWorkflow):
//     - gen (PersistentSession) runs; the seeding fake writes a transcript file
//     under the engine-derived SessionDir → ReadTreeAt(SessionDir) captures the
//     whole projects/ subtree as a gzip-tar → node.completed carries session_ref
//     → RunState.SessionRefs["gen"] populated.
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
//     Backend.WriteTreeAt with the committed subtree tar before launching.
//     This proves the full capture → fold → restore pipeline end-to-end.
//
// Structure mirrors testSnapshotRestoreCalledOnResume (conformance/snapshot.go):
// shared InMemoryBlobs across run/resume/proof, FailExecAfterN to halt the frontier,
// run-fake / resume-fake recorded for assertions.
func testSessionRefFullE2E(t *testing.T) {
	t.Helper()
	blobs := state.NewInMemoryBlobs()

	// Session adapter: PersistentSession + IsolatedConfigDir (mirrors the real
	// anthropic/claude-code-session caps). The engine derives the per-run config
	// dir + SessionDir from Caps.StagingRoot + RunID alone.
	sessionAdapter := agentfake.New("test/session-e2e-agent").
		WithCaps(agent.Caps{NativeSchema: true, PersistentSession: true, IsolatedConfigDir: true}).
		Script(0, agentfake.Result{}). // run 1: gen succeeds
		Script(1, agentfake.Result{})  // proof dispatch: gen succeeds again

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

	// Track run/resume fakes for assertions. The engine computes
	// SessionDir = /work/.awf/claude-session/<RunID>/projects (fake StagingRoot
	// is /work/.awf); the seeding fake writes a transcript file under whatever
	// SessionDir the engine derives, so the test needs no a-priori RunID.
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
		return &sessionSeedingFake{Fake: f, transcript: []byte(`{"session":"e2e-run1"}`)}
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

	// The subtree tar blob must survive in shared blobs after run + resume.
	transcriptTar, getErr := blobs.Get(genSessionRef)
	if getErr != nil {
		t.Fatalf("session-subtree blob not durable after resume: %v", getErr)
	}

	// Restore-path proof (mirrors the snapshot test's "Restore was called" assertion):
	// Dispatch gen again using the fully-folded RunState (after both run + resume).
	// The folded RunState still has SessionRefs["gen"] from run 1's commit (downstream's
	// completion on resume does not clear gen's SessionRef).
	// The fresh fake must call WriteTreeAt at the SessionDir with the committed tar.
	finalEvents := mustFoldEvents(t, h)
	finalRS, fferr := engine.Fold(finalEvents, blobs)
	if fferr != nil {
		t.Fatalf("Fold after resume: %v", fferr)
	}
	if finalRS.SessionRefs["gen"] == "" {
		t.Fatalf("RunState.SessionRefs[gen] empty after resume Fold — session_ref should survive (it was not invalidated)")
	}

	// Build a fresh fake (as if gen were re-dispatched on a new run with prior session).
	// No pre-seed: the restore path (WriteTreeAt) populates the SessionDir subtree
	// before capture's ReadTreeAt reads it back.
	proofFake := container.NewFake().WithBlobs(blobs)
	ph, phErr := proofFake.Create(t.Context(), container.ContainerSpec{Name: "ws"})
	if phErr != nil {
		t.Fatalf("Create proof fake: %v", phErr)
	}

	// Register the session adapter for the proof dispatch (Script(1) = gen's second call).
	proofReg := &agent.Registry{}
	if regErr := proofReg.Register(sessionAdapter); regErr != nil {
		t.Fatalf("Register session adapter for proof: %v", regErr)
	}

	const proofSessionDir = "/work/.awf/claude-session/proof/projects"
	proofRI := engine.ResolvedInputs{
		Uses:       "test/session-e2e-agent",
		SessionDir: proofSessionDir,
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

	// WriteTreeAt must have been called at the SessionDir with the original committed tar.
	var restored bool
	for _, c := range proofFake.WriteTreeAtCalls {
		if c.Dir == proofSessionDir && bytes.Equal(c.Content, transcriptTar) {
			restored = true
		}
	}
	if !restored {
		t.Errorf("restore-path proof: WriteTreeAt not called at %q with committed subtree tar (WriteTreeAtCalls=%+v)", proofSessionDir, proofFake.WriteTreeAtCalls)
	}
}

// testSessionRefRestoreCallsWriteTreeAt verifies the restore site: when RunState
// carries a SessionRef for the dispatched node path and ResolvedInputs has a
// SessionDir, the dispatcher calls Backend.WriteTreeAt with the committed
// subtree tar before launching the agent.
func testSessionRefRestoreCallsWriteTreeAt(t *testing.T) {
	t.Helper()
	blobs := state.NewInMemoryBlobs()
	f := container.NewFake().WithBlobs(blobs)
	h, err := f.Create(t.Context(), container.ContainerSpec{Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Seed a session-subtree tar blob and a SessionRef for node path "gen".
	tarGz, terr := container.BuildTreeTar(map[string][]byte{"-work/s.jsonl": []byte(`{"session":"round1"}`)})
	if terr != nil {
		t.Fatalf("BuildTreeTar: %v", terr)
	}
	ref, putErr := blobs.Put(tarGz)
	if putErr != nil {
		t.Fatalf("blobs.Put: %v", putErr)
	}
	rs := engine.NewRunState("run-1", "digest-1", nil)
	rs.SessionRefs["gen"] = ref

	// Dispatch the agent with RunState + Blobs set on the dispatcher.
	const sessionDir = "/work/.awf/claude-session/run-1/projects"
	dr := dispatchOKAgentSessionForConformance(t, f, h, rs, blobs, engine.ResolvedInputs{
		SessionDir: sessionDir,
	})
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q (Err: %v)", dr.Outcome, engine.OutcomeOK, dr.Err)
	}

	// WriteTreeAt must have been called at SessionDir with the committed tar.
	var found bool
	for _, c := range f.WriteTreeAtCalls {
		if c.Dir == sessionDir && bytes.Equal(c.Content, tarGz) {
			found = true
		}
	}
	if !found {
		t.Errorf("restore did not call WriteTreeAt with the committed subtree tar (WriteTreeAtCalls=%+v)", f.WriteTreeAtCalls)
	}

	// The subtree blob survives in the shared store (content-addressed durability).
	got, getErr := blobs.Get(ref)
	if getErr != nil {
		t.Errorf("session-subtree blob not durable after restore: %v", getErr)
	}
	if !bytes.Equal(got, tarGz) {
		t.Errorf("blob round-trip mismatch")
	}
}

// testSessionRefNoRestoreOnFirstRun verifies that when RunState has no
// SessionRefs entry for the dispatched node path, WriteTreeAt is NOT called
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

	// Pre-seed a file UNDER SessionDir so the capture block (ReadTreeAt) succeeds
	// and the step returns OutcomeOK. WriteFileAt does not touch WriteTreeAtCalls,
	// so WriteTreeAt remains observably uncalled (restore is the only WriteTreeAt caller).
	const sessionDir = "/work/.awf/claude-session/run-1/projects"
	if wErr := f.WriteFileAt(t.Context(), h, sessionDir+"/-work/s.jsonl", []byte("transcript")); wErr != nil {
		t.Fatalf("seed transcript: %v", wErr)
	}

	dr := dispatchOKAgentSessionForConformance(t, f, h, rs, blobs, engine.ResolvedInputs{
		SessionDir: sessionDir,
	})
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q (Err: %v)", dr.Outcome, engine.OutcomeOK, dr.Err)
	}
	if len(f.WriteTreeAtCalls) != 0 {
		t.Errorf("WriteTreeAt called on first run (no prior session), want 0 calls; got %+v", f.WriteTreeAtCalls)
	}
}

// testSessionRefBlobRoundTrip verifies the full Commit + Fold path: a
// DispatchResult with a SessionTranscript (the subtree tar) is content-addressed
// by Commit, recorded as SessionRef in NodeCompletedData, and Fold populates
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

	transcriptTar, terr := container.BuildTreeTar(map[string][]byte{"-work/s.jsonl": []byte(`{"session":"round2"}`)})
	if terr != nil {
		t.Fatalf("BuildTreeTar: %v", terr)
	}
	dr := engine.DispatchResult{Outcome: engine.OutcomeOK, SessionTranscript: transcriptTar}

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

	// The blob survives and matches the original subtree tar.
	got, getErr := blobs.Get(ref)
	if getErr != nil {
		t.Fatalf("blobs.Get(SessionRef): %v", getErr)
	}
	if !bytes.Equal(got, transcriptTar) {
		t.Errorf("blob round-trip mismatch")
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
