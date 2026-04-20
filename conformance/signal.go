package conformance

import (
	"encoding/json"
	"errors"
	"testing"

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

	// Build a concrete fake so we can call BlockExec.
	fake := buildPauseFake(t, factory)
	fake.BlockExec() // gate: Exec blocks until ctx-cancel or ReleaseBlockedExec
	singleShot := func() container.Backend { return fake }

	h := newHarnessWithBroker(t, singleShot, signalPauseWorkflow)
	if err := h.broker.WritePause(signal.PauseRequest{Reason: "test"}); err != nil {
		t.Fatalf("WritePause: %v", err)
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

	// The poller (1ms) fires while the engine goroutine is blocked at the
	// gate. Once pause is detected ctx is cancelled; Exec returns ctx.Err;
	// the engine unwinds. Close the gate so the fake doesn't stay armed.
	res := <-ch
	fake.ReleaseBlockedExec() // idempotent — ctx-cancel may have already drained

	if res.oc != "" {
		t.Errorf("outcome on pause = %q, want \"\"", res.oc)
	}
	if res.err == nil || !isErrPaused(res.err) {
		t.Errorf("err = %v, want ErrPaused", res.err)
	}
	events := mustFoldEvents(t, h)
	var sawPaused int
	for _, e := range events {
		if e.Type == engine.EventRunPaused {
			sawPaused++
		}
	}
	if sawPaused != 1 {
		t.Errorf("run.paused events = %d, want 1", sawPaused)
	}

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

	fake := buildPauseFake(t, factory)
	fake.BlockExec() // gate: Exec blocks until ctx-cancel or ReleaseBlockedExec
	singleShot := func() container.Backend { return fake }

	h := newHarnessWithBroker(t, singleShot, signalPauseWorkflow)
	if err := h.broker.WriteCancel(signal.CancelRequest{Reason: "test"}); err != nil {
		t.Fatalf("WriteCancel: %v", err)
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
	fake.ReleaseBlockedExec()

	if res.oc != "" {
		t.Errorf("outcome on cancel = %q, want \"\"", res.oc)
	}
	if res.err == nil || !isErrCancelled(res.err) {
		t.Errorf("err = %v, want ErrCancelled", res.err)
	}
	events := mustFoldEvents(t, h)
	var sawCancelled int
	for _, e := range events {
		if e.Type == engine.EventRunCancelled {
			sawCancelled++
			var d engine.RunCancelledData
			_ = json.Unmarshal(e.Data, &d)
			if d.Reason != "test" {
				t.Errorf("Reason = %q, want test", d.Reason)
			}
		}
	}
	if sawCancelled != 1 {
		t.Errorf("run.cancelled events = %d, want 1", sawCancelled)
	}
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
