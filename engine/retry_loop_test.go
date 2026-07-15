package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/state"
)

// stubDispatcher returns a programmed sequence of DispatchResult/Err pairs,
// one per call to Run.
type stubDispatcher struct {
	calls   int
	results []stubResult
}

type stubResult struct {
	dr  engine.DispatchResult
	err error
}

func (s *stubDispatcher) Run(_ context.Context, _ engine.NodeIntent) (engine.DispatchResult, <-chan container.IOChunk, error) {
	if s.calls >= len(s.results) {
		return engine.DispatchResult{}, nil, errors.New("stubDispatcher: ran out of programmed results")
	}
	r := s.results[s.calls]
	s.calls++
	ch := make(chan container.IOChunk)
	close(ch)
	return r.dr, ch, r.err
}

func defaultIntent() engine.NodeIntent {
	return engine.NodeIntent{
		Path: "x",
		Node: &ir.CodeStep{ID: "x"},
	}
}

func TestRunWithRetrySuccessOnFirstAttempt(t *testing.T) {
	t.Parallel()
	exit := 0
	dsp := &stubDispatcher{results: []stubResult{
		{dr: engine.DispatchResult{Outcome: engine.OutcomeOK, ExitCode: &exit}},
	}}
	log := state.NewInMemoryLog(clock.System{})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	policy := retry.Policy{Attempts: 3, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}

	dr, _, err := engine.RunWithRetry(context.Background(), dsp, defaultIntent(), policy, clk, log)
	if err != nil {
		t.Fatalf("RunWithRetry: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", dr.Outcome)
	}
	if dsp.calls != 1 {
		t.Errorf("dispatcher called %d times, want 1", dsp.calls)
	}
	if clk.Now() != time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) {
		t.Errorf("clock advanced to %v, want unchanged", clk.Now())
	}
	events, _ := log.Fold()
	for _, e := range events {
		if e.Type == engine.EventRetryAttempt {
			t.Errorf("found retry.attempt event after one-shot success: %+v", e)
		}
	}
}

func TestRunWithRetryRetryThenSuccess(t *testing.T) {
	t.Parallel()
	exit := 0
	dsp := &stubDispatcher{results: []stubResult{
		{dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure, Err: errors.New("transient")}},
		{dr: engine.DispatchResult{Outcome: engine.OutcomeOK, ExitCode: &exit}},
	}}
	log := state.NewInMemoryLog(clock.System{})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	policy := retry.Policy{Attempts: 3, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}

	dr, _, err := engine.RunWithRetry(context.Background(), dsp, defaultIntent(), policy, clk, log)
	if err != nil {
		t.Fatalf("RunWithRetry: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", dr.Outcome)
	}
	if dsp.calls != 2 {
		t.Errorf("dispatcher called %d times, want 2", dsp.calls)
	}
	// One preceding sleep before attempt 2 = EffectiveBackoff(2, no-hint, path).
	wantTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(policy.EffectiveBackoff(2, 0, "x"))
	if !clk.Now().Equal(wantTime) {
		t.Errorf("clock = %v, want %v", clk.Now(), wantTime)
	}
	events, _ := log.Fold()
	var attempts []engine.RetryAttemptData
	for _, e := range events {
		if e.Type == engine.EventRetryAttempt {
			var d engine.RetryAttemptData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal retry.attempt: %v", err)
			}
			attempts = append(attempts, d)
		}
	}
	if len(attempts) != 1 {
		t.Fatalf("len(retry.attempt) = %d, want 1; full = %+v", len(attempts), attempts)
	}
	if attempts[0].N != 1 {
		t.Errorf("attempt N = %d, want 1", attempts[0].N)
	}
	if attempts[0].Outcome != string(engine.OutcomeRetryableFailure) {
		t.Errorf("attempt Outcome = %q, want %q", attempts[0].Outcome, engine.OutcomeRetryableFailure)
	}
	if attempts[0].Error == "" {
		t.Error("attempt Error is empty; want 'transient'")
	}
}

func TestRunWithRetryExhaustedReturnsLastError(t *testing.T) {
	t.Parallel()
	dsp := &stubDispatcher{results: []stubResult{
		{dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure, Err: errors.New("first")}},
		{dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure, Err: errors.New("second")}},
		{dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure, Err: errors.New("third")}},
	}}
	log := state.NewInMemoryLog(clock.System{})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	policy := retry.Policy{Attempts: 3, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}

	dr, _, err := engine.RunWithRetry(context.Background(), dsp, defaultIntent(), policy, clk, log)
	if err == nil {
		t.Fatal("RunWithRetry should return an error after exhaustion, got nil")
	}
	if err.Error() != "third" {
		t.Errorf("err = %v, want 'third' (the last attempt's error)", err)
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %v, want retryable", dr.Outcome)
	}
	if dsp.calls != 3 {
		t.Errorf("dispatcher called %d times, want 3", dsp.calls)
	}
	events, _ := log.Fold()
	var attempts int
	for _, e := range events {
		if e.Type == engine.EventRetryAttempt {
			attempts++
		}
	}
	if attempts != 2 {
		t.Errorf("retry.attempt events = %d, want 2 (attempts 1 and 2; attempt 3 halts via run-error)", attempts)
	}
	// Two preceding sleeps (before attempts 2 and 3), each EffectiveBackoff w/ jitter.
	wantTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(policy.EffectiveBackoff(2, 0, "x")).
		Add(policy.EffectiveBackoff(3, 0, "x"))
	if !clk.Now().Equal(wantTime) {
		t.Errorf("clock = %v, want %v", clk.Now(), wantTime)
	}
}

func TestRunWithRetryPermanentStopsImmediately(t *testing.T) {
	t.Parallel()
	exit := 78
	dsp := &stubDispatcher{results: []stubResult{
		{dr: engine.DispatchResult{Outcome: engine.OutcomePermanentFailure, ExitCode: &exit, Err: errors.New("misconfig")}},
	}}
	log := state.NewInMemoryLog(clock.System{})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	policy := retry.Policy{Attempts: 3, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}

	dr, _, err := engine.RunWithRetry(context.Background(), dsp, defaultIntent(), policy, clk, log)
	if err == nil {
		t.Fatal("RunWithRetry should propagate the permanent error, got nil")
	}
	if dr.Outcome != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent", dr.Outcome)
	}
	if dsp.calls != 1 {
		t.Errorf("dispatcher called %d times, want 1 (permanent → no retry)", dsp.calls)
	}
	if !clk.Now().Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("clock advanced on permanent failure: %v", clk.Now())
	}
}

func TestRunWithRetryRejectsInconsistentDispatchResults(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	tests := []struct {
		name      string
		dr        engine.DispatchResult
		directErr error
	}{
		{name: "ok with cause", dr: engine.DispatchResult{Outcome: engine.OutcomeOK, Err: cause}},
		{name: "retryable without cause", dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure}},
		{name: "permanent without cause", dr: engine.DispatchResult{Outcome: engine.OutcomePermanentFailure}},
		{name: "empty result without direct error", dr: engine.DispatchResult{}},
		{name: "typed outcome with direct error", dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure}, directErr: errors.New("direct")},
		{name: "direct error with result cause", dr: engine.DispatchResult{Err: cause}, directErr: errors.New("direct")},
		{name: "direct error with outputs", dr: engine.DispatchResult{Outputs: map[string]any{"leaked": true}}, directErr: errors.New("direct")},
		{name: "unknown outcome", dr: engine.DispatchResult{Outcome: engine.Outcome("mystery"), Err: cause}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dsp := &stubDispatcher{results: []stubResult{{dr: tt.dr, err: tt.directErr}}}
			log := state.NewInMemoryLog(clock.System{})
			clk := &clock.Fake{}
			policy := retry.Policy{Attempts: 1}

			dr, _, err := engine.RunWithRetry(context.Background(), dsp, defaultIntent(), policy, clk, log)
			if err == nil || !strings.Contains(err.Error(), "engine.RunWithRetry") {
				t.Fatalf("err = %v, want RunWithRetry invariant error", err)
			}
			if dr.Outcome != "" {
				t.Errorf("Outcome = %q, want empty on internal error", dr.Outcome)
			}
		})
	}
}

func TestRunWithRetryRecoveryContinueRejectsMixedLiveReplayTuple(t *testing.T) {
	t.Parallel()
	dsp := &stubDispatcher{results: []stubResult{{
		dr: engine.DispatchResult{
			Outcome: engine.OutcomeRetryableFailure,
			Err:     errors.New("typed cause"),
		},
		err: fmt.Errorf("wrapped: %w", agent.ErrLiveReplayRequired),
	}}}
	log := state.NewInMemoryLog(clock.System{})
	clk := &clock.Fake{}
	policy := retry.Policy{Attempts: 2, Recovery: "continue"}

	dr, _, err := engine.RunWithRetry(context.Background(), dsp, defaultIntent(), policy, clk, log)
	if err == nil || !strings.Contains(err.Error(), "engine.RunWithRetry") {
		t.Fatalf("err = %v, want RunWithRetry invariant error", err)
	}
	if dr.Outcome != "" {
		t.Errorf("Outcome = %q, want empty on internal error", dr.Outcome)
	}
	if dsp.calls != 1 {
		t.Errorf("dispatcher called %d times, want 1", dsp.calls)
	}
}

// TestRunWithRetryHonorsRetryAfterHint verifies that a retryable attempt
// carrying a server Retry-After hint (DispatchResult.RetryAfter) makes the loop
// wait at least that long before the next attempt — instead of the much shorter
// exp curve. This is what stops AWF from burning its retry budget hammering a
// rate-limit window the server said resets in 45s.
func TestRunWithRetryHonorsRetryAfterHint(t *testing.T) {
	t.Parallel()
	exit := 0
	dsp := &stubDispatcher{results: []stubResult{
		{dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure, Err: errors.New("rate_limited"), RetryAfter: 45 * time.Second}},
		{dr: engine.DispatchResult{Outcome: engine.OutcomeOK, ExitCode: &exit}},
	}}
	log := state.NewInMemoryLog(clock.System{})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := &clock.Fake{T: start}
	// Curve for attempt 2 is only 1s — the 45s hint must dominate.
	policy := retry.Policy{Attempts: 3, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}

	dr, _, err := engine.RunWithRetry(context.Background(), dsp, defaultIntent(), policy, clk, log)
	if err != nil {
		t.Fatalf("RunWithRetry: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", dr.Outcome)
	}
	want := start.Add(policy.EffectiveBackoff(2, 45*time.Second, "x"))
	if !clk.Now().Equal(want) {
		t.Errorf("clock = %v, want %v (honored the 45s Retry-After hint, not the 1s curve)", clk.Now(), want)
	}
	if clk.Now().Sub(start) < 45*time.Second {
		t.Errorf("slept %v, want >= 45s (the hint was not honored)", clk.Now().Sub(start))
	}
}

func TestRunWithRetryContextCancelInSleep(t *testing.T) {
	t.Parallel()
	dsp := &stubDispatcher{results: []stubResult{
		{dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure, Err: errors.New("first")}},
		// Second result is unreachable — ctx cancelled during sleep before attempt 2.
		{dr: engine.DispatchResult{Outcome: engine.OutcomeOK}},
	}}
	log := state.NewInMemoryLog(clock.System{})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	policy := retry.Policy{Attempts: 3, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := engine.RunWithRetry(ctx, dsp, defaultIntent(), policy, clk, log)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestRunWithRetryUnderSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exit := 0
		dsp := &stubDispatcher{results: []stubResult{
			{dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure, Err: errors.New("first")}},
			{dr: engine.DispatchResult{Outcome: engine.OutcomeRetryableFailure, Err: errors.New("second")}},
			{dr: engine.DispatchResult{Outcome: engine.OutcomeOK, ExitCode: &exit}},
		}}
		log := state.NewInMemoryLog(clock.System{})
		policy := retry.Policy{Attempts: 5, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}

		start := time.Now()
		dr, _, err := engine.RunWithRetry(context.Background(), dsp, defaultIntent(), policy, clock.System{}, log)
		if err != nil {
			t.Fatalf("RunWithRetry: %v", err)
		}
		if dr.Outcome != engine.OutcomeOK {
			t.Errorf("Outcome = %v, want ok", dr.Outcome)
		}
		// Two retries → two preceding sleeps, each EffectiveBackoff (curve+jitter),
		// fully deterministic (jitter is a pure hash, no rand).
		want := policy.EffectiveBackoff(2, 0, "x") + policy.EffectiveBackoff(3, 0, "x")
		if elapsed := time.Since(start); elapsed != want {
			t.Errorf("elapsed = %v, want %v (under synctest = deterministic)", elapsed, want)
		}
	})
}

// TestRunWithRetryViaLocalDispatcher is the integration test the rest of slice
// 2.4 is missing — it composes the real LocalDispatcher with a real
// container.Fake (via FailExecAfterN to induce a transport-class failure on
// the first attempt) and RunWithRetry. Without this, slice 2.4 ships two
// halves of the dispatch-and-retry composition with no test that they fit
// together (Revision #5).
func TestRunWithRetryViaLocalDispatcher(t *testing.T) {
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fake.FailExecAfterN(0)
	fake.ProgramExec("./flaky.sh", container.ExecResult{
		ExitCode: 0,
		Stdout:   []byte("recovered\n"),
	}, nil)

	d := &engine.LocalDispatcher{
		Backend: fake,
		Handles: map[string]container.Handle{"lab": h},
	}
	intent := engine.NodeIntent{
		Path: "flaky",
		Node: &ir.CodeStep{ID: "flaky", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command: "./flaky.sh",
		},
	}
	log := state.NewInMemoryLog(clock.System{})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	policy := retry.Policy{Attempts: 3, Backoff: retry.BackoffExp, Initial: time.Second, Max: 60 * time.Second}

	dr, chunks, err := engine.RunWithRetry(context.Background(), d, intent, policy, clk, log)
	if err != nil {
		t.Fatalf("RunWithRetry: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", dr.Outcome)
	}
	if string(dr.Stdout) != "recovered\n" {
		t.Errorf("Stdout = %q, want %q", dr.Stdout, "recovered\n")
	}
	if len(fake.Calls) != 2 {
		t.Errorf("fake.Calls len = %d, want 2", len(fake.Calls))
	}
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(policy.EffectiveBackoff(2, 0, "flaky"))
	if !clk.Now().Equal(want) {
		t.Errorf("clock = %v, want %v (one EffectiveBackoff(2) sleep between attempts)", clk.Now(), want)
	}
	events, _ := log.Fold()
	var retryAttempts int
	for _, e := range events {
		if e.Type == engine.EventRetryAttempt {
			retryAttempts++
		}
	}
	if retryAttempts != 1 {
		t.Errorf("retry.attempt event count = %d, want 1", retryAttempts)
	}
	if chunks != nil {
		for range chunks {
		}
	}
}
