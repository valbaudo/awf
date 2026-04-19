package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// Parallel handler tests share the scriptedDispatcher / scriptedResult /
// tryTestRig helpers from engine/try_test.go — same package, same test-
// dispatch pattern. scriptedResult's ctxAware flag is what makes parallel
// branches' sibling-cancellation testable (mirrors container.Fake's pre-Run
// ctx.Err() short-circuit).

// ----- serializingLog tests -----

func TestSerializingLogConcurrentAppend(t *testing.T) {
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	inner := state.NewInMemoryLog(clk)
	wrapped := newSerializingLog(inner)

	const N = 64
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			if err := wrapped.Append(state.Event{
				Type: "test.event",
				Path: fmt.Sprintf("branch-%d", i),
				Data: []byte(fmt.Sprintf(`{"i":%d}`, i)),
			}); err != nil {
				t.Errorf("concurrent Append %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	events, _ := wrapped.Fold()
	if len(events) != N {
		t.Errorf("Fold len = %d, want %d", len(events), N)
	}
	seen := map[uint64]bool{}
	for _, e := range events {
		if seen[e.Seq] {
			t.Errorf("duplicate Seq=%d", e.Seq)
		}
		seen[e.Seq] = true
	}
	for s := uint64(1); s <= N; s++ {
		if !seen[s] {
			t.Errorf("Seq=%d missing", s)
		}
	}
}

// ----- runParallel tests -----

func TestRunParallelAllBranchesOK(t *testing.T) {
	par := &ir.Parallel{Children: ir.NodeList{
		&ir.CodeStep{ID: "b0", Run: "echo a", Container: "ca"},
		&ir.CodeStep{ID: "b1", Run: "echo b", Container: "cb"},
		&ir.CodeStep{ID: "b2", Run: "echo c", Container: "cc"},
	}}
	disp, lg, blobs := tryTestRig(t, map[string]scriptedResult{
		"b0": {outcome: OutcomeOK}, "b1": {outcome: OutcomeOK}, "b2": {outcome: OutcomeOK},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{par}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runParallel(context.Background(), par, "parallel[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("all-ok: got (%q, %v), want (ok, nil)", oc, err)
	}
	for _, id := range []string{"parallel[0].b0", "parallel[0].b1", "parallel[0].b2"} {
		if _, done := rs.LookupCompleted(id); !done {
			t.Errorf("branch %q not in Completed", id)
		}
	}
}

func TestRunParallelOneBranchFailsCancelsSiblings(t *testing.T) {
	par := &ir.Parallel{Children: ir.NodeList{
		&ir.CodeStep{ID: "b0-fail", Run: "exit 1", Container: "ca", Retry: &ir.RetryPolicy{Attempts: 1}},
		&ir.CodeStep{ID: "b1", Run: "sleep", Container: "cb", Retry: &ir.RetryPolicy{Attempts: 1}},
		&ir.CodeStep{ID: "b2", Run: "sleep", Container: "cc", Retry: &ir.RetryPolicy{Attempts: 1}},
	}}
	b0Err := errors.New("b0 exhausted retries")
	disp, lg, blobs := tryTestRig(t, map[string]scriptedResult{
		"b0-fail": {outcome: OutcomeRetryableFailure, err: b0Err},
		"b1":      {outcome: OutcomeRetryableFailure, err: context.Canceled, ctxAware: true},
		"b2":      {outcome: OutcomeRetryableFailure, err: context.Canceled, ctxAware: true},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{par}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runParallel(context.Background(), par, "parallel[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if oc != OutcomeRetryableFailure {
		t.Errorf("outcome = %q, want retryable_failure", oc)
	}
	if err == nil || !strings.Contains(err.Error(), "b0 exhausted retries") {
		t.Errorf("err = %v, want contains 'b0 exhausted retries' (lowest-index wins)", err)
	}
}

func TestRunParallelDeterministicFirstError(t *testing.T) {
	par := &ir.Parallel{Children: ir.NodeList{
		&ir.CodeStep{ID: "b0", Run: "exit 1", Container: "ca", Retry: &ir.RetryPolicy{Attempts: 1}},
		&ir.CodeStep{ID: "b1", Run: "echo", Container: "cb", Retry: &ir.RetryPolicy{Attempts: 1}},
		&ir.CodeStep{ID: "b2", Run: "exit 2", Container: "cc", Retry: &ir.RetryPolicy{Attempts: 1}},
	}}
	disp, _, _ := tryTestRig(t, map[string]scriptedResult{
		"b0": {outcome: OutcomeRetryableFailure, err: errors.New("b0 err")},
		"b1": {outcome: OutcomeOK},
		"b2": {outcome: OutcomeRetryableFailure, err: errors.New("b2 err")},
	})
	for i := 0; i < 20; i++ {
		clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		lg := state.NewInMemoryLog(clk)
		blobs := state.NewInMemoryBlobs()
		wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{par}}
		rs := NewRunState("run-x", "digest", nil)
		_, err := runParallel(context.Background(), par, "parallel[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "b0 err") {
			t.Errorf("iter %d: err = %v, want contains 'b0 err'", i, err)
		}
	}
}

func TestRunParallelSkipInBranch(t *testing.T) {
	par := &ir.Parallel{Children: ir.NodeList{
		&ir.CodeStep{ID: "b0", Run: "echo", Container: "ca"},
		&ir.Skip{Reason: "skip middle"},
		&ir.CodeStep{ID: "b2", Run: "echo", Container: "cc"},
	}}
	disp, lg, blobs := tryTestRig(t, map[string]scriptedResult{
		"b0": {outcome: OutcomeOK}, "b2": {outcome: OutcomeOK},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{par}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runParallel(context.Background(), par, "parallel[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("skip-in-branch: got (%q, %v), want (ok, nil)", oc, err)
	}
	for _, id := range []string{"parallel[0].b0", "parallel[0].b2"} {
		if _, done := rs.LookupCompleted(id); !done {
			t.Errorf("branch %q not in Completed (skip in sibling shouldn't have cancelled it)", id)
		}
	}
	events, _ := lg.Fold()
	nSkipped := 0
	for _, e := range events {
		if e.Type == EventNodeSkipped {
			nSkipped++
		}
	}
	if nSkipped != 1 {
		t.Errorf("node.skipped count = %d, want 1", nSkipped)
	}
}

func TestRunParallelCtxCancelRunsSiblingFinally(t *testing.T) {
	par := &ir.Parallel{Children: ir.NodeList{
		&ir.CodeStep{ID: "b0-fail", Run: "exit 1", Container: "ca", Retry: &ir.RetryPolicy{Attempts: 1}},
		&ir.Try{
			Do: ir.NodeList{
				&ir.CodeStep{ID: "b1-do", Run: "sleep", Container: "cb", Retry: &ir.RetryPolicy{Attempts: 1}},
			},
			Finally: ir.NodeList{
				&ir.CodeStep{ID: "b1-finally", Run: "echo", Container: "cb", Retry: &ir.RetryPolicy{Attempts: 1}},
			},
		},
	}}
	disp, lg, blobs := tryTestRig(t, map[string]scriptedResult{
		"b0-fail":    {outcome: OutcomeRetryableFailure, err: errors.New("b0 err")},
		"b1-do":      {outcome: OutcomeRetryableFailure, err: context.Canceled, ctxAware: true},
		"b1-finally": {outcome: OutcomeOK},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{par}}
	rs := NewRunState("run-x", "digest", nil)
	_, _ = runParallel(context.Background(), par, "parallel[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if _, done := rs.LookupCompleted("parallel[0].try[1].finally.b1-finally"); !done {
		t.Errorf("b1-finally not in Completed; finally must run even on ctx-cancel")
	}
}

func TestRunParallelCrossBranchRefIsRaceTolerant(t *testing.T) {
	// Spec §5.4 doesn't forbid {{ step.<sibling>.<field> }} from a parallel
	// branch. Behavior is non-deterministic by design:
	//   * producer races ahead → template substitutes successfully →
	//     dispatcher called for "consumer" → script returns ok.
	//   * producer hasn't committed → template eval fails (AWF4002
	//     "not yet committed") → consumer fails with permanent_failure
	//     per slice 2.5 DQ7 (dispatcher NEVER called for "consumer").
	// Critical property: NO PANIC under -race (Task 1's runstate mutex
	// + Scope.Resolve routing through LookupCompleted prevent the
	// concurrent-map-read+write that would panic without protection).
	par := &ir.Parallel{Children: ir.NodeList{
		&ir.CodeStep{ID: "producer", Run: "echo p", Container: "ca"},
		&ir.CodeStep{ID: "consumer", Run: "echo {{ step.producer.exit_code }}", Container: "cb"},
	}}
	disp, lg, blobs := tryTestRig(t, map[string]scriptedResult{
		"producer": {outcome: OutcomeOK},
		"consumer": {outcome: OutcomeOK},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{par}}
	rs := NewRunState("run-x", "digest", nil)
	oc, err := runParallel(context.Background(), par, "parallel[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil)
	if oc != OutcomeOK && oc != OutcomePermanentFailure {
		t.Errorf("cross-branch-ref-race: outcome = %q, want ok or permanent_failure", oc)
	}
	_ = err // err is fine either way (nil on ok; non-nil template-eval on permanent_failure)
}

func TestRunParallelSiblingCancelInterruptsRetrySleepUnderSynctest(t *testing.T) {
	// Design §C invariant: when a parallel branch fails, siblings observe
	// ctx-cancel — including DURING retry backoff sleeps. This is the
	// load-bearing property: without it, a failing branch's siblings
	// would burn their full retry budget before the parallel returned.
	//
	// We assert via retry.attempt event count, NOT via elapsed time:
	//   * b0 has Attempts=1 (fails immediately, no retries).
	//   * b1 has Attempts=3 (defaults: exp backoff, 1s initial, 60s max
	//     via retry.Merge). Without ctx-cancel: 2 retry.attempt events
	//     (between attempts 1→2 and 2→3). With ctx-cancel mid-sleep:
	//     ≤ 1 retry.attempt event.
	// Asserting < 2 catches a regression where retry sleeps become
	// ctx-unaware (b1 would burn full backoff budget).
	synctest.Test(t, func(t *testing.T) {
		par := &ir.Parallel{Children: ir.NodeList{
			&ir.CodeStep{ID: "b0-fail", Run: "exit 1", Container: "ca",
				Retry: &ir.RetryPolicy{Attempts: 1}},
			&ir.CodeStep{ID: "b1-transient", Run: "fail", Container: "cb",
				Retry: &ir.RetryPolicy{Attempts: 3}}, // defaults: exp/1s/60s via retry.Default
		}}
		disp, lg, blobs := tryTestRig(t, map[string]scriptedResult{
			"b0-fail":      {outcome: OutcomeRetryableFailure, err: errors.New("b0 err")},
			"b1-transient": {outcome: OutcomeRetryableFailure, err: errors.New("transient")},
		})
		wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{par}}
		rs := NewRunState("run-x", "digest", nil)
		_, _ = runParallel(context.Background(), par, "parallel[0]", wf, rs, disp, lg, blobs, clock.System{}, nil, nil)

		events, _ := lg.Fold()
		b1Retries := 0
		for _, e := range events {
			if e.Type == EventRetryAttempt && e.Path == "parallel[0].b1-transient" {
				b1Retries++
			}
		}
		if b1Retries >= 2 {
			t.Errorf("b1-transient emitted %d retry.attempt events; ctx-cancel from b0 should have interrupted before attempt 3 (would otherwise be 2 events)", b1Retries)
		}
	})
}
