package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
)

// newTestLog constructs an InMemoryLog for controls_test helpers.
func newTestLog(t *testing.T) *state.InMemoryLog {
	t.Helper()
	return state.NewInMemoryLog(&clock.Fake{T: time.Now()})
}

// tempBroker constructs a Broker rooted at a t.TempDir()-derived control
// directory. NOT the production layout — production uses
// .awf/runs/<run.id>/control. The flat layout is fine for unit tests
// because each test gets its own t.TempDir().
func tempBroker(t *testing.T) *signal.Broker {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "control")
	return signal.NewBroker(dir, signal.WithPollInterval(time.Millisecond))
}

func TestPollControlsDetectsCancel(t *testing.T) {
	b := tempBroker(t)
	if err := b.WriteCancel(signal.CancelRequest{Reason: "ctest"}); err != nil {
		t.Fatalf("WriteCancel: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rs := NewRunState("r", "d", nil)
	done := make(chan struct{})
	go pollControls(ctx, b, rs, cancel, done)

	select {
	case <-done:
		// poller exited.
	case <-time.After(time.Second):
		t.Fatal("poller did not exit within 1s of cancel.json present")
	}
	if !rs.IsCancelled() {
		t.Errorf("IsCancelled = false; want true")
	}
	if rs.LookupCancelReason() != "ctest" {
		t.Errorf("CancelReason = %q, want \"ctest\"", rs.LookupCancelReason())
	}
	if ctx.Err() == nil {
		t.Errorf("ctx not cancelled by poller")
	}
}

func TestPollControlsDetectsPause(t *testing.T) {
	b := tempBroker(t)
	if err := b.WritePause(signal.PauseRequest{NodePath: "step.x", Reason: "test"}); err != nil {
		t.Fatalf("WritePause: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rs := NewRunState("r", "d", nil)
	done := make(chan struct{})
	go pollControls(ctx, b, rs, cancel, done)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poller did not exit within 1s")
	}
	pm := rs.LookupPaused()
	if pm == nil || pm.NodePath != "step.x" || pm.Reason != "test" {
		t.Errorf("LookupPaused = %+v", pm)
	}
	if ctx.Err() == nil {
		t.Errorf("ctx not cancelled by poller")
	}
}

func TestPollControlsCancelWinsOverPause(t *testing.T) {
	b := tempBroker(t)
	_ = b.WritePause(signal.PauseRequest{})
	_ = b.WriteCancel(signal.CancelRequest{Reason: "cancel"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rs := NewRunState("r", "d", nil)
	done := make(chan struct{})
	go pollControls(ctx, b, rs, cancel, done)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poller did not exit")
	}
	if !rs.IsCancelled() {
		t.Errorf("IsCancelled = false; cancel should have won")
	}
	if rs.LookupPaused() != nil {
		// Defense-in-depth — the poller returns immediately on cancel detection,
		// so it should NOT have set Paused even if pause.json was scanned first.
		// CheckPauseCancel returns BOTH; pollControls handles cancel first.
		t.Errorf("LookupPaused = %+v; cancel-wins resolution failed", rs.LookupPaused())
	}
}

func TestPollControlsExitsOnCtxCancel(t *testing.T) {
	// No pause/cancel file → poller should keep polling until parent ctx
	// cancels (engine.Run's defer cancel() fires when Run returns).
	b := tempBroker(t)
	ctx, cancel := context.WithCancel(context.Background())
	rs := NewRunState("r", "d", nil)
	done := make(chan struct{})
	go pollControls(ctx, b, rs, cancel, done)

	// Cancel ctx after a short delay; poller should return.
	time.AfterFunc(10*time.Millisecond, cancel)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poller did not exit on parent ctx cancel")
	}
	if rs.IsCancelled() || rs.LookupPaused() != nil {
		t.Errorf("poller set flags despite no control file: %v %+v", rs.IsCancelled(), rs.LookupPaused())
	}
}

func TestAppendRunPausedRoundTrip(t *testing.T) {
	log := newTestLog(t)
	if err := appendRunPaused(log, "step.x", "test"); err != nil {
		t.Fatalf("appendRunPaused: %v", err)
	}
	events, _ := log.Fold()
	for _, e := range events {
		if e.Type == EventRunPaused {
			return // pass
		}
	}
	t.Errorf("no run.paused event in log")
}

func TestAppendRunCancelledRoundTrip(t *testing.T) {
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	if err := appendRunCancelled(log, "test"); err != nil {
		t.Fatalf("appendRunCancelled: %v", err)
	}
	events, _ := log.Fold()
	for _, e := range events {
		if e.Type == EventRunCancelled {
			return // pass
		}
	}
	t.Errorf("no run.cancelled event in log")
}

// H1 fix: real unit tests of appendTerminalControlEvents (the extracted
// helper). Replaces the prior t.Skip'd "late-cancel" placeholder — these
// tests pin the state-machine behavior directly without timing dependencies.
//
// Three behaviors to pin:
//  1. Cancelled flag → run.cancelled appended + ErrCancelled returned.
//  2. Paused marker → run.paused appended + ErrPaused returned.
//  3. Neither flag → no event, nil returned.
//
// Cancel-wins precedence is exercised by setting both flags.

func TestAppendTerminalControlEvents_CancelledFlag(t *testing.T) {
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	rs := NewRunState("r", "d", nil)
	rs.SetCancelled(true)
	rs.SetCancelReason("operator stop")
	err := appendTerminalControlEvents(log, rs)
	if !errors.Is(err, signal.ErrCancelled) {
		t.Errorf("err = %v, want signal.ErrCancelled", err)
	}
	events, _ := log.Fold()
	var foundCancelled bool
	for _, e := range events {
		if e.Type == EventRunCancelled {
			foundCancelled = true
			var d RunCancelledData
			_ = json.Unmarshal(e.Data, &d)
			if d.Reason != "operator stop" {
				t.Errorf("Reason = %q, want %q", d.Reason, "operator stop")
			}
		}
	}
	if !foundCancelled {
		t.Errorf("no run.cancelled event in log")
	}
}

func TestAppendTerminalControlEvents_PausedMarker(t *testing.T) {
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	rs := NewRunState("r", "d", nil)
	rs.SetPaused(&PauseMarker{Reason: "operator break"})
	err := appendTerminalControlEvents(log, rs)
	if !errors.Is(err, signal.ErrPaused) {
		t.Errorf("err = %v, want signal.ErrPaused", err)
	}
	events, _ := log.Fold()
	var foundPaused bool
	for _, e := range events {
		if e.Type == EventRunPaused {
			foundPaused = true
		}
	}
	if !foundPaused {
		t.Errorf("no run.paused event in log")
	}
}

func TestAppendTerminalControlEvents_NeitherFlagReturnsNil(t *testing.T) {
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	rs := NewRunState("r", "d", nil)
	err := appendTerminalControlEvents(log, rs)
	if err != nil {
		t.Errorf("err = %v, want nil (no flags set)", err)
	}
	events, _ := log.Fold()
	if len(events) != 0 {
		t.Errorf("events = %d, want 0 (no flags → no emission)", len(events))
	}
}

func TestAppendTerminalControlEvents_CancelWinsOverPause(t *testing.T) {
	// If both flags are set (e.g. pause arrived then cancel), cancel takes
	// precedence (terminal beats non-terminal). Matches pollControls'
	// resolution order.
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	rs := NewRunState("r", "d", nil)
	rs.SetPaused(&PauseMarker{Reason: "earlier pause"})
	rs.SetCancelled(true)
	rs.SetCancelReason("later cancel")
	err := appendTerminalControlEvents(log, rs)
	if !errors.Is(err, signal.ErrCancelled) {
		t.Errorf("err = %v, want signal.ErrCancelled (cancel wins)", err)
	}
	events, _ := log.Fold()
	var sawPaused, sawCancelled bool
	for _, e := range events {
		if e.Type == EventRunPaused {
			sawPaused = true
		}
		if e.Type == EventRunCancelled {
			sawCancelled = true
		}
	}
	if sawPaused {
		t.Errorf("run.paused emitted; cancel-wins should have suppressed it")
	}
	if !sawCancelled {
		t.Errorf("run.cancelled not emitted")
	}
}
