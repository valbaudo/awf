package conformance

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/signal"
)

// testSignal runs Bucket 8 sub-tests for the signal subsystem (Phase 3
// slice 3.5). Naming convention matches Bucket 7 (signal_ prefix; M2 fix):
//
//   - signal_await_delivers: write a signal BEFORE running; the run consumes
//     it on first poll without blocking; one signal.received + two
//     node.completed (await + after) in the log.
//   - signal_resume_replays: write a signal; run completes; resume on a
//     bare fake (no programs) — committed events replay, nothing
//     re-executes. (M1 rename: was early_signal_persists_across_resume,
//     which over-claimed; mid-commit fault injection needs a richer harness
//     that Phase 3 minimum doesn't provide — TODO for a future slice.)
//   - signal_pause_halts: write pause.json BEFORE running; pollControls
//     detects it after step a's commit; engine appends run.paused +
//     returns ErrPaused. ClearPauseCancel + resume continues from step b.
//   - signal_cancel_terminal: write cancel.json; engine appends terminal
//     run.cancelled; ErrCancelled returned.
func testSignal(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("signal_await_delivers", func(t *testing.T) { testSignalAwaitDelivers(t, factory) })
	t.Run("signal_resume_replays", func(t *testing.T) { testSignalResumeReplays(t, factory) })
	t.Run("signal_pause_halts", func(t *testing.T) { testSignalPauseHalts(t, factory) })
	t.Run("signal_cancel_terminal", func(t *testing.T) { testSignalCancelTerminal(t, factory) })
	t.Run("signal_keyed_match", func(t *testing.T) { testSignalKeyedMatch(t, factory) })
}

// testSignalKeyedMatch is the SP4 keyed-signals (await where:) conformance.
// A map over two hypotheses ({id:a}, {id:b}) each await `oob-hit` WHERE
// candidate_id matches the item's own id. Two signals are pre-written in an
// order that does NOT match item order (b first, a second) so a plain
// earliest-first consume would give item "a" the "b" payload (and item "b"
// would then time out on the fixture's 2s bound — fast-fail, not a hang).
//
// The load-bearing assertion is per-item CORRELATION, not a payload count: for
// each signal.received event we map its path's item-N suffix to the over[]
// hypothesis id, resolve the event's PayloadRef from Blobs, and assert the
// delivered candidate_id equals that item's own id. Counting events alone would
// pass under a regression that ignores where: (both signals get consumed,
// merely SWAPPED) — verified empirically (see commit message). This assertion
// FAILS under that swap and PASSES only when correlation holds.
func testSignalKeyedMatch(t *testing.T, factory BackendFactory) {
	t.Helper()
	h := newHarnessWithInput(t, factory, signalWhereWorkflow, map[string]any{
		"hyps": []any{
			map[string]any{"id": "a"},
			map[string]any{"id": "b"},
		},
	})
	// newHarnessWithInput does not wire a broker; derive controlDir from the
	// shared baseDir exactly like newHarnessWithBroker (M13 layout).
	controlDir := filepath.Join(h.baseDir, "control")
	h.broker = signal.NewBroker(controlDir, signal.WithPollInterval(time.Millisecond))

	// Pre-write TWO signals in an order that does NOT match item order: the
	// "b" hit arrives first (seq 1), "a" second (seq 2). Keyed where: must
	// correlate each to the right item regardless of seq.
	if _, err := h.broker.WriteSignal("oob-hit", []byte(`{"candidate_id":"b","hit":true}`)); err != nil {
		t.Fatalf("WriteSignal b: %v", err)
	}
	if _, err := h.broker.WriteSignal("oob-hit", []byte(`{"candidate_id":"a","hit":true}`)); err != nil {
		t.Fatalf("WriteSignal a: %v", err)
	}

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("signal_keyed_match: err = %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("outcome = %q, want ok", oc)
	}

	// over[] index → the hypothesis id at that map item. The item-N suffix in a
	// signal.received path is the over[] index, so item-0 must have consumed the
	// "a" hit and item-1 the "b" hit, regardless of seq order.
	wantByItem := map[int]string{0: "a", 1: "b"}

	events := mustFoldEvents(t, h)
	gotByItem := map[int]string{}
	for _, e := range events {
		if e.Type != engine.EventSignalReceived {
			continue
		}
		itemN, ok := itemIndexFromPath(e.Path)
		if !ok {
			t.Fatalf("signal.received path %q has no item-N suffix", e.Path)
		}
		var d engine.SignalReceivedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("unmarshal SignalReceivedData at %q: %v", e.Path, err)
		}
		if d.PayloadRef == "" {
			t.Fatalf("signal.received at %q has empty PayloadRef", e.Path)
		}
		raw, err := h.blobs.Get(d.PayloadRef)
		if err != nil {
			t.Fatalf("blobs.Get(%q) at %q: %v", d.PayloadRef, e.Path, err)
		}
		var payload struct {
			CandidateID string `json:"candidate_id"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("unmarshal payload at %q: %v", e.Path, err)
		}
		gotByItem[itemN] = payload.CandidateID
	}

	if len(gotByItem) != len(wantByItem) {
		t.Fatalf("signal.received events = %d, want %d (one per correlated item)", len(gotByItem), len(wantByItem))
	}
	for itemN, want := range wantByItem {
		if got := gotByItem[itemN]; got != want {
			t.Errorf("item-%d consumed candidate_id %q, want %q (mis-correlated signal)", itemN, got, want)
		}
	}
}

// itemIndexFromPath extracts the over[] index N from a map-body node path's
// trailing ".item-N.<step>" segment (e.g. "map[0].item-1.wait_oob" -> 1).
// Returns ok=false if no item segment is present.
func itemIndexFromPath(path string) (int, bool) {
	for _, seg := range strings.Split(path, ".") {
		if rest, found := strings.CutPrefix(seg, "item-"); found {
			n, err := strconv.Atoi(rest)
			if err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func testSignalAwaitDelivers(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: `echo "true"`, res: container.ExecResult{ExitCode: 0}},
	})
	h := newHarnessWithBroker(t, programmedFactory, signalAwaitWorkflow)
	// Pre-write the signal — first Receive poll picks it up.
	if _, err := h.broker.WriteSignal("human_review", []byte(`{"approved":true}`)); err != nil {
		t.Fatalf("WriteSignal: %v", err)
	}
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("Bucket 8 await_delivers: err = %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("outcome = %q, want ok", oc)
	}
	events := mustFoldEvents(t, h)
	var sigReceived int
	for _, e := range events {
		if e.Type == engine.EventSignalReceived {
			sigReceived++
		}
	}
	if sigReceived != 1 {
		t.Errorf("signal.received events = %d, want 1", sigReceived)
	}
}

func testSignalResumeReplays(t *testing.T, factory BackendFactory) {
	t.Helper()
	// Phase 3 slice 3.5 minimum (M1 fix): pre-write signal; run completes;
	// resume against a bare fake; committed events replay-via-Completed
	// short-circuit. This sub-test pins "committed-signal events replay
	// correctly" — NOT the "mid-commit crash recovery" half-commit case
	// (which is tested by engine/signal_step_test.go's
	// TestRunSignalStep_HalfCommitResume; the harness lacks a clean way to
	// simulate a crash AFTER signal.received but BEFORE node.completed —
	// see TODO at end of this function).
	programmedFactory := preProgramFake(t, factory, []execProgram{
		{cmd: `echo "true"`, res: container.ExecResult{ExitCode: 0}},
	})
	h := newHarnessWithBroker(t, programmedFactory, signalAwaitWorkflow)
	if _, err := h.broker.WriteSignal("human_review", []byte(`{"approved":true}`)); err != nil {
		t.Fatalf("WriteSignal: %v", err)
	}
	oc, err := h.runWorkflow(t)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("round-1: oc=%q err=%v", oc, err)
	}

	// Pre-resume: verify signal.received in log.
	preEvents := mustFoldEvents(t, h)
	var preSig int
	for _, e := range preEvents {
		if e.Type == engine.EventSignalReceived {
			preSig++
		}
	}
	if preSig != 1 {
		t.Fatalf("pre-resume signal.received = %d, want 1", preSig)
	}

	// Resume against a BARE fake (no programs). The committed await is
	// REPLAYED (LookupCompleted short-circuits in runSignalStep); the
	// after step is also already completed; resume does nothing.
	h.factory = factory
	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Errorf("resume: err = %v", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Errorf("resume outcome = %q, want ok", oc2)
	}

	// TODO(future-slice): the half-commit-resume case (signal.received
	// journaled, node.completed missing) cannot be cleanly produced via
	// the current harness — needs a broker fault hook or an engine fault
	// injection point. The engine-level unit test
	// TestRunSignalStep_HalfCommitResume covers the same code path.
}

// signal_pause_halts + signal_cancel_terminal are TIMING-SENSITIVE: the
// pollControls goroutine must fire BEFORE the workflow completes all its
// steps. With the fake backend executing steps in nanoseconds the 1ms
// poll interval would race past all steps before detecting the control file.
//
// Fix: use container.Fake.BlockExec() to gate the first Exec call. The
// engine goroutine blocks inside Exec on the gate channel; the poller
// goroutine (1ms ticker, real time) fires, reads the pre-written control
// file, sets the runstate flag, and cancels ctx. Exec then returns
// ctx.Err — the engine unwinds and appends the terminal event.
//
// Pattern:
//  1. Build a *Fake directly (so we hold a reference for BlockExec).
//  2. Program it and wrap it in a single-shot factory.
//  3. Call fake.BlockExec() → gate channel.
//  4. Write the control file (pause.json / cancel.json).
//  5. Launch runWorkflow in a goroutine.
//  6. Wait for completion (the poller cancels ctx → Exec unblocks via
//     ctx.Done; gate channel is closed so subsequent Execs proceed if any).
//  7. Assert outcomes and log events.

func testSignalPauseHalts(t *testing.T, factory BackendFactory) {
	t.Helper()
	h := runHaltedScenario(t, factory, func(h *harness) error {
		return h.broker.WritePause(signal.PauseRequest{Reason: "test"})
	}, isErrPaused, engine.EventRunPaused, "ErrPaused")

	// Resume scenario: ClearPauseCancel + a fresh (fully programmed) factory.
	_ = h.broker.ClearPauseCancel()
	h.factory = preProgramFake(t, factory, []execProgram{
		{cmd: "echo a", res: container.ExecResult{ExitCode: 0}},
		{cmd: "echo b", res: container.ExecResult{ExitCode: 0}},
		{cmd: "echo c", res: container.ExecResult{ExitCode: 0}},
	})
	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Errorf("resume after pause: err = %v", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Errorf("resume outcome = %q, want ok", oc2)
	}
}

func testSignalCancelTerminal(t *testing.T, factory BackendFactory) {
	t.Helper()
	h := runHaltedScenario(t, factory, func(h *harness) error {
		return h.broker.WriteCancel(signal.CancelRequest{Reason: "test"})
	}, isErrCancelled, engine.EventRunCancelled, "ErrCancelled")

	// Verify the cancel reason round-trips through the log.
	events := mustFoldEvents(t, h)
	for _, e := range events {
		if e.Type == engine.EventRunCancelled {
			var d engine.RunCancelledData
			_ = json.Unmarshal(e.Data, &d)
			if d.Reason != "test" {
				t.Errorf("Reason = %q, want test", d.Reason)
			}
		}
	}
}

// runHaltedScenario is the shared scaffolding for signal_pause_halts and
// signal_cancel_terminal. Both tests:
//  1. Build a *Fake pre-programmed with the signalPauseWorkflow steps.
//  2. Arm BlockExec so the engine goroutine blocks on the first Exec call.
//  3. Write the control file (pause.json or cancel.json) — writeControl
//     does this in a closure so each caller picks Pause vs Cancel.
//  4. Launch runWorkflow in a goroutine; collect (outcome, err) via a chan.
//  5. The 1ms poller fires while Exec is gated → detects the file →
//     cancels ctx → Exec returns ctx.Err → engine unwinds + emits the
//     terminal event.
//  6. Assert outcome=="" + err matches the expected sentinel + exactly one
//     event of haltedEventType appears in the log.
//
// Returns the harness so callers can run follow-up assertions (e.g. the
// resume-after-pause path in testSignalPauseHalts).
func runHaltedScenario(
	t *testing.T,
	factory BackendFactory,
	writeControl func(*harness) error,
	matchErr func(error) bool,
	haltedEventType string,
	wantSentinelName string,
) *harness {
	t.Helper()
	fake := buildPauseFake(t, factory)
	fake.BlockExec() // gate: Exec blocks until ctx-cancel or ReleaseBlockedExec
	singleShot := func() container.Backend { return fake }

	h := newHarnessWithBroker(t, singleShot, signalPauseWorkflow)
	if err := writeControl(h); err != nil {
		t.Fatalf("write control file: %v", err)
	}

	type result struct {
		oc  engine.Outcome
		err error
	}
	ch := make(chan result, 1)
	go func() {
		oc, err := h.runWorkflow(t)
		ch <- result{oc, err}
	}()
	res := <-ch
	fake.ReleaseBlockedExec() // idempotent — ctx-cancel may have already drained

	if res.oc != "" {
		t.Errorf("outcome = %q, want \"\"", res.oc)
	}
	if res.err == nil || !matchErr(res.err) {
		t.Errorf("err = %v, want %s", res.err, wantSentinelName)
	}
	events := mustFoldEvents(t, h)
	var sawEvent int
	for _, e := range events {
		if e.Type == haltedEventType {
			sawEvent++
		}
	}
	if sawEvent != 1 {
		t.Errorf("%s events = %d, want 1", haltedEventType, sawEvent)
	}
	return h
}

// buildPauseFake constructs a *container.Fake pre-programmed with the three
// echo steps in signalPauseWorkflow. Returns the concrete *Fake (not Backend)
// so callers can invoke BlockExec. Falls back to a bare factory() if it isn't
// a *Fake (non-fake backend conformance; timing control not needed there).
func buildPauseFake(t *testing.T, factory BackendFactory) *container.Fake {
	t.Helper()
	b := factory()
	fake, ok := b.(*container.Fake)
	if !ok {
		t.Skip("signal pause/cancel BlockExec gate requires container.Fake")
	}
	fake.ProgramExec("echo a", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo b", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo c", container.ExecResult{ExitCode: 0}, nil)
	return fake
}

// isErrPaused / isErrCancelled match the typed sentinels via errors.Is.
// Trivial helpers extracted for readability inside the sub-tests above.
func isErrPaused(err error) bool    { return errors.Is(err, signal.ErrPaused) }
func isErrCancelled(err error) bool { return errors.Is(err, signal.ErrCancelled) }
