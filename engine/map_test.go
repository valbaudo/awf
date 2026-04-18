package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// newMapRig builds the inputs needed for runMap tests: a real LocalDispatcher
// backed by a real container.Fake, with one entry in Handles for the map's
// declared container name. Mirrors gateTestDispatcher's setup but uses real
// types because Design Q2 requires *LocalDispatcher.
func newMapRig(t *testing.T, mapContainerName string) (*LocalDispatcher, *container.Fake, *state.InMemoryLog, *state.InMemoryBlobs) {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	// Pre-create a "base" handle for the map's declared container name.
	// The handler doesn't use this directly — it Creates per-item handles —
	// but LocalDispatcher's Handles map must have the key for the validator
	// pathway. (In production CLI flow, the CLI pre-creates one handle per
	// declared container; the map handler then layers per-item handles via
	// WithItemHandle.)
	baseHandle, err := fake.Create(context.Background(), container.ContainerSpec{Name: mapContainerName})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	ld := &LocalDispatcher{
		Backend: fake,
		Handles: map[string]container.Handle{
			mapContainerName: baseHandle,
		},
	}
	return ld, fake, state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
}

// staticOverWorkflow builds a Workflow with a top-level input and a Map node.
// Helper to keep tests focused on handler behavior.
func staticOverWorkflow(asName string, overItems []any, body ir.NodeList, mapContainer string, concurrency int, minSuccess *ir.Ratio) *ir.Workflow {
	_ = overItems // value provided to NewRunState by callers; the map's over expr reads it via {{ input.items }}
	mapNode := &ir.Map{
		Over:        ir.Expr("{{ input.items }}"),
		As:          asName,
		Container:   mapContainer,
		Concurrency: concurrency,
		MinSuccess:  minSuccess,
		Body:        body,
	}
	return &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{
			mapContainer: {Image: "oci://example.com/r@sha256:" + strings.Repeat("0", 64)},
		},
		Input: &ir.JSONSchema{
			"type": "object",
			"properties": map[string]any{
				"items": map[string]any{"type": "array"},
			},
		},
		Graph: ir.NodeList{mapNode},
	}
}

func TestRunMapEmptyOver(t *testing.T) {
	wf := staticOverWorkflow("cve", nil, ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "echo {{ cve }}", Container: "c0"},
	}, "c0", 1, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	ld, _, lg, blobs := newMapRig(t, "c0")
	rs := NewRunState("run-x", "digest", map[string]any{"items": []any{}})

	oc, err := runMap(context.Background(), mapNode, "map[0]", wf, rs, ld, lg, blobs, &clock.Fake{}, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("empty over: got (%q, %v), want (ok, nil)", oc, err)
	}
	// No map.item events (no items dispatched).
	events, _ := lg.Fold()
	for _, e := range events {
		if e.Type == EventMapItem {
			t.Errorf("unexpected map.item event with empty over: %+v", e)
		}
	}
}

func TestRunMapSingleItemPasses(t *testing.T) {
	fake := container.NewFake()
	fake.ProgramExec("echo cve-1", container.ExecResult{ExitCode: 0}, nil)
	baseHandle, _ := fake.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld := &LocalDispatcher{
		Backend: fake,
		Handles: map[string]container.Handle{"c0": baseHandle},
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	wf := staticOverWorkflow("cve", []any{"cve-1"}, ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "echo {{ cve }}", Container: "c0"},
	}, "c0", 1, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState("run-x", "digest", map[string]any{"items": []any{"cve-1"}})

	oc, err := runMap(context.Background(), mapNode, "map[0]", wf, rs, ld, lg, blobs, clk, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("got (%q, %v), want (ok, nil)", oc, err)
	}
	items := rs.LookupMapItems("map[0]")
	if len(items) != 1 || items[0].Status != ItemPassed {
		t.Errorf("MapItems = %+v, want 1 passed", items)
	}
}

func TestRunMapMultipleItemsAllPass(t *testing.T) {
	fake := container.NewFake()
	fake.ProgramExec("echo a", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo b", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo c", container.ExecResult{ExitCode: 0}, nil)
	baseHandle, _ := fake.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld := &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"c0": baseHandle}}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	wf := staticOverWorkflow("x", []any{"a", "b", "c"}, ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "echo {{ x }}", Container: "c0"},
	}, "c0", 2, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState("run-x", "digest", map[string]any{"items": []any{"a", "b", "c"}})

	oc, err := runMap(context.Background(), mapNode, "map[0]", wf, rs, ld, lg, blobs, clk, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("got (%q, %v), want (ok, nil)", oc, err)
	}
	items := rs.LookupMapItems("map[0]")
	if len(items) != 3 {
		t.Fatalf("MapItems len = %d, want 3", len(items))
	}
	for _, mr := range items {
		if mr.Status != ItemPassed {
			t.Errorf("item N=%d status=%q, want item_passed", mr.N, mr.Status)
		}
	}
}

func TestRunMapConcurrencyCapEnforced(t *testing.T) {
	// concurrency: 2; 5 items, each item's exec blocks until released.
	// Assert at most 2 concurrent in-flight at any time.
	fake := container.NewFake()
	var inflight int64
	var maxInflight int64
	release := make(chan struct{})
	// Custom Backend that counts inflight on Exec, blocks until release.
	cb := &countingBackend{
		Fake:        fake,
		release:     release,
		inflight:    &inflight,
		maxInflight: &maxInflight,
	}
	baseHandle, _ := cb.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld := &LocalDispatcher{Backend: cb, Handles: map[string]container.Handle{"c0": baseHandle}}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	wf := staticOverWorkflow("x", []any{"a", "b", "c", "d", "e"}, ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "echo {{ x }}", Container: "c0"},
	}, "c0", 2, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState("run-x", "digest", map[string]any{"items": []any{"a", "b", "c", "d", "e"}})

	done := make(chan struct{})
	go func() {
		_, _ = runMap(context.Background(), mapNode, "map[0]", wf, rs, ld, lg, blobs, clk, nil)
		close(done)
	}()
	// Wait until in-flight reaches 2 (or timeout).
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&maxInflight) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(release) // unblocks all goroutines
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runMap did not return within 5s")
	}
	if got := atomic.LoadInt64(&maxInflight); got != 2 {
		t.Errorf("maxInflight = %d, want 2 (concurrency cap)", got)
	}
}

// countingBackend embeds *container.Fake (promoting Create / Destroy /
// CaptureFiles / Snapshot / Restore) and overrides only Exec to count
// concurrent in-flight calls (tracking max) and block until release is
// closed. M9 simplification: ~15 LOC instead of 50 (no per-method stubs).
//
// Note: tests holding a *countingBackend instance must access the embedded
// *Fake via the field (`cb.Fake`) if they want to call ProgramExec etc. —
// the embedding only promotes Backend-interface methods.
type countingBackend struct {
	*container.Fake
	release     chan struct{}
	inflight    *int64
	maxInflight *int64
}

func (b *countingBackend) Exec(ctx context.Context, h container.Handle, cmd container.Cmd) (container.ExecResult, <-chan container.IOChunk, error) {
	cur := atomic.AddInt64(b.inflight, 1)
	for {
		m := atomic.LoadInt64(b.maxInflight)
		if cur <= m || atomic.CompareAndSwapInt64(b.maxInflight, m, cur) {
			break
		}
	}
	<-b.release
	atomic.AddInt64(b.inflight, -1)
	closed := make(chan container.IOChunk)
	close(closed)
	return container.ExecResult{ExitCode: 0}, closed, nil
}

func TestRunMapMinSuccessTolerates(t *testing.T) {
	// 3 items, min_success: 2; one fails, two pass → map ends ok.
	fake := container.NewFake()
	fake.ProgramExec("echo a", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo b", container.ExecResult{ExitCode: 1}, nil) // fails
	fake.ProgramExec("echo c", container.ExecResult{ExitCode: 0}, nil)
	baseHandle, _ := fake.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld := &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"c0": baseHandle}}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	minSuccess := ir.Ratio("2")
	wf := staticOverWorkflow("x", []any{"a", "b", "c"}, ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "echo {{ x }}", Container: "c0", Retry: &ir.RetryPolicy{Attempts: 1}},
	}, "c0", 3, &minSuccess)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState("run-x", "digest", map[string]any{"items": []any{"a", "b", "c"}})

	oc, err := runMap(context.Background(), mapNode, "map[0]", wf, rs, ld, lg, blobs, clk, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("min_success met: got (%q, %v), want (ok, nil)", oc, err)
	}
	// Verify success/fail counts.
	items := rs.LookupMapItems("map[0]")
	var pass, fail int
	for _, mr := range items {
		switch mr.Status {
		case ItemPassed:
			pass++
		case ItemFailed:
			fail++
		}
	}
	if pass != 2 || fail != 1 {
		t.Errorf("pass=%d fail=%d; want pass=2 fail=1", pass, fail)
	}
}

func TestRunMapMinSuccessFailsBelow(t *testing.T) {
	// 3 items, min_success: 3 (default = all); one fails → map fails.
	fake := container.NewFake()
	fake.ProgramExec("echo a", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo b", container.ExecResult{ExitCode: 1}, nil)
	fake.ProgramExec("echo c", container.ExecResult{ExitCode: 0}, nil)
	baseHandle, _ := fake.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld := &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"c0": baseHandle}}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	wf := staticOverWorkflow("x", []any{"a", "b", "c"}, ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "echo {{ x }}", Container: "c0", Retry: &ir.RetryPolicy{Attempts: 1}},
	}, "c0", 3, nil) // nil MinSuccess → default = all (3)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState("run-x", "digest", map[string]any{"items": []any{"a", "b", "c"}})

	oc, err := runMap(context.Background(), mapNode, "map[0]", wf, rs, ld, lg, blobs, clk, nil)
	if oc == OutcomeOK {
		t.Errorf("default min_success not met: got ok, want non-ok")
	}
	if err == nil {
		t.Errorf("err = nil, want non-nil")
	}
}

func TestRunMapSkipInItemEndsAsOK(t *testing.T) {
	// Item-1's body contains a Skip; that item ends as item_passed (skip ends
	// the item as ok per design §E step 5). The other items pass normally.
	// MinSuccess default = all → map ends ok.
	fake := container.NewFake()
	fake.ProgramExec("echo a", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo c", container.ExecResult{ExitCode: 0}, nil)
	baseHandle, _ := fake.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld := &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"c0": baseHandle}}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	// Body: an if-statement that runs Skip on item-1 (x == "b"), else runs echo.
	// Phase 3 templating supports == in if.cond.
	body := ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ x == \"b\" }}"),
			Then: ir.NodeList{&ir.Skip{Reason: "skip middle"}},
			Else: ir.NodeList{&ir.CodeStep{ID: "echo", Run: "echo {{ x }}", Container: "c0"}},
		},
	}
	wf := staticOverWorkflow("x", []any{"a", "b", "c"}, body, "c0", 3, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState("run-x", "digest", map[string]any{"items": []any{"a", "b", "c"}})

	oc, err := runMap(context.Background(), mapNode, "map[0]", wf, rs, ld, lg, blobs, clk, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("skip-in-item: got (%q, %v), want (ok, nil)", oc, err)
	}
	items := rs.LookupMapItems("map[0]")
	if len(items) != 3 {
		t.Fatalf("MapItems len = %d, want 3", len(items))
	}
	for _, mr := range items {
		if mr.Status != ItemPassed {
			t.Errorf("item N=%d status=%q, want item_passed (skip → ok)", mr.N, mr.Status)
		}
	}
}

// seedRunStartedWithInput appends a run.started event to lg carrying an
// InputRef that points at a freshly-Put input blob. Lets the round-2 Fold
// reconstruct rs2.Input via the realistic CLI path (no manual
// rs2.Input = ... hack). Returns the input map the test should also seed
// into rs1 for round-1's NewRunState call.
//
// HI3: replaces the prior approach of manually overriding rs2.Input
// post-Fold; the runtime invariant "Fold reconstructs Input from
// run.started's InputRef" (engine/fold.go:86-98) is exercised end-to-end.
func seedRunStartedWithInput(t *testing.T, lg state.Log, blobs state.Blobs, input map[string]any) {
	t.Helper()
	inputBytes, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	inputRef, err := blobs.Put(inputBytes)
	if err != nil {
		t.Fatalf("put input blob: %v", err)
	}
	runStartedData, err := json.Marshal(RunStartedData{
		RunID:          "run-x",
		WorkflowDigest: "digest",
		InputRef:       inputRef,
	})
	if err != nil {
		t.Fatalf("marshal run.started: %v", err)
	}
	if err := lg.Append(state.Event{Type: EventRunStarted, Data: runStartedData}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
}

func TestRunMapResumeReplaysCommittedItems(t *testing.T) {
	// Per spec §8 + Design Q6: committed items are REPLAYED on resume,
	// not re-executed. Round 1 commits all 3 as item_passed; round 2
	// resumes against a BARE fake (no programmed Exec entries). If
	// runMap correctly skips committed items, no Exec calls happen on
	// round 2 → resume completes ok. If it incorrectly re-executes,
	// the bare fake errors them → the assertion catches it via
	// round2Fake.Calls len > 0.

	// Round 1: program all 3 to succeed.
	fake1 := container.NewFake()
	fake1.ProgramExec("echo a", container.ExecResult{ExitCode: 0}, nil)
	fake1.ProgramExec("echo b", container.ExecResult{ExitCode: 0}, nil)
	fake1.ProgramExec("echo c", container.ExecResult{ExitCode: 0}, nil)
	baseHandle1, _ := fake1.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld1 := &LocalDispatcher{Backend: fake1, Handles: map[string]container.Handle{"c0": baseHandle1}}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()

	// HI3: realistic Fold path — InputRef-seeded run.started lets round-2
	// Fold reconstruct rs2.Input without manual override.
	input := map[string]any{"items": []any{"a", "b", "c"}}
	seedRunStartedWithInput(t, lg, blobs, input)

	wf := staticOverWorkflow("x", []any{"a", "b", "c"}, ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "echo {{ x }}", Container: "c0"},
	}, "c0", 3, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs1 := NewRunState("run-x", "digest", input)

	oc1, err1 := runMap(context.Background(), mapNode, "map[0]", wf, rs1, ld1, lg, blobs, clk, nil)
	if oc1 != OutcomeOK || err1 != nil {
		t.Fatalf("round-1: got (%q, %v), want (ok, nil)", oc1, err1)
	}
	pre := rs1.LookupMapItems("map[0]")
	if len(pre) != 3 {
		t.Fatalf("pre-resume MapItems len = %d, want 3", len(pre))
	}

	// Round 2: simulate resume via Fold. Use a BARE fake — no programs.
	// runMap MUST skip all 3 committed items; any Exec call would error.
	events, _ := lg.Fold()
	rs2, ferr := Fold(events, blobs)
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}
	// Sanity: Fold reconstructed Input from InputRef.
	if rs2.Input == nil {
		t.Fatal("Fold did not reconstruct rs2.Input from InputRef (HI3 fix did not take effect)")
	}
	fake2 := container.NewFake() // NO programs — any Exec call will error.
	baseHandle2, _ := fake2.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld2 := &LocalDispatcher{Backend: fake2, Handles: map[string]container.Handle{"c0": baseHandle2}}

	oc2, err2 := runMap(context.Background(), mapNode, "map[0]", wf, rs2, ld2, lg, blobs, clk, nil)
	if oc2 != OutcomeOK || err2 != nil {
		t.Fatalf("resume: got (%q, %v), want (ok, nil) — committed items must replay, not re-execute", oc2, err2)
	}

	// The load-bearing assertion: fake2 received ZERO Exec calls beyond
	// the one Create. Any body re-execution would hit fake2.Exec → error
	// (no program) → propagated. Count via fake2.Calls.
	if len(fake2.Calls) != 0 {
		t.Errorf("resume re-executed body steps: fake2.Calls = %v, want [] (committed items should replay, not re-run)", fake2.Calls)
	}

	// Verify the post-resume RunState has all 3 items still passed.
	post := rs2.LookupMapItems("map[0]")
	if len(post) != 3 {
		t.Fatalf("post-resume MapItems len = %d, want 3", len(post))
	}
	for _, mr := range post {
		if mr.Status != ItemPassed {
			t.Errorf("post-resume N=%d status=%q, want item_passed (Q6 contract: committed status is preserved)", mr.N, mr.Status)
		}
	}
}

func TestRunMapResumeFailedItemsStayFailed(t *testing.T) {
	// Per Design Q6: failed items are FINAL across resume — they are
	// committed as item_failed and resume SKIPS them. Pin this contract
	// by:
	//   Round 1: 2 succeed + 1 fail with min_success: 3 → map fails.
	//   Round 2: resume on a fresh fake with ALL 3 programmed to succeed.
	//   Assertion: resume returns the SAME OutcomeRetryableFailure; no
	//   item re-executes (round-2 fake.Calls is empty).
	//
	// CALLER NOTE: This exercises engine.runMap directly. Via the CLI,
	// `awf resume` refuses runs with terminal run.finished events (slice
	// 2.6 refusal); failed runs aren't user-resumable through that path.
	// This test pins the engine-level Q6 contract for callers that bypass
	// the refusal (the conformance harness; future tooling).

	// Round 1: program a, b, c — b returns ExitCode 1 (item_failed).
	fake1 := container.NewFake()
	fake1.ProgramExec("echo a", container.ExecResult{ExitCode: 0}, nil)
	fake1.ProgramExec("echo b", container.ExecResult{ExitCode: 1}, nil)
	fake1.ProgramExec("echo c", container.ExecResult{ExitCode: 0}, nil)
	baseHandle1, _ := fake1.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld1 := &LocalDispatcher{Backend: fake1, Handles: map[string]container.Handle{"c0": baseHandle1}}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()

	input := map[string]any{"items": []any{"a", "b", "c"}}
	seedRunStartedWithInput(t, lg, blobs, input)

	minSuccess := ir.Ratio("3") // require all 3
	wf := staticOverWorkflow("x", []any{"a", "b", "c"}, ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "echo {{ x }}", Container: "c0", Retry: &ir.RetryPolicy{Attempts: 1}},
	}, "c0", 3, &minSuccess)
	mapNode := wf.Graph[0].(*ir.Map)
	rs1 := NewRunState("run-x", "digest", input)

	oc1, err1 := runMap(context.Background(), mapNode, "map[0]", wf, rs1, ld1, lg, blobs, clk, nil)
	if oc1 != OutcomeRetryableFailure {
		t.Fatalf("round-1: outcome = %q, want OutcomeRetryableFailure (2 pass, 1 fail, min_success: 3)", oc1)
	}
	if err1 == nil {
		t.Fatal("round-1: err = nil, want non-nil")
	}
	pre := rs1.LookupMapItems("map[0]")
	var prePass, preFail int
	for _, mr := range pre {
		switch mr.Status {
		case ItemPassed:
			prePass++
		case ItemFailed:
			preFail++
		}
	}
	if prePass != 2 || preFail != 1 {
		t.Fatalf("round-1: pre-resume pass=%d fail=%d, want 2/1", prePass, preFail)
	}

	// Round 2: fresh fake with ALL 3 programmed to succeed. If item-1
	// gets a second chance (per a hypothetical "retry on resume" bug),
	// it would now succeed → map would return ok. Q6 says NO retry —
	// resume returns the SAME failure.
	events, _ := lg.Fold()
	rs2, ferr := Fold(events, blobs)
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}
	fake2 := container.NewFake()
	fake2.ProgramExec("echo a", container.ExecResult{ExitCode: 0}, nil)
	fake2.ProgramExec("echo b", container.ExecResult{ExitCode: 0}, nil) // would-pass on retry
	fake2.ProgramExec("echo c", container.ExecResult{ExitCode: 0}, nil)
	baseHandle2, _ := fake2.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld2 := &LocalDispatcher{Backend: fake2, Handles: map[string]container.Handle{"c0": baseHandle2}}

	oc2, err2 := runMap(context.Background(), mapNode, "map[0]", wf, rs2, ld2, lg, blobs, clk, nil)
	if oc2 != OutcomeRetryableFailure {
		t.Errorf("resume: outcome = %q, want OutcomeRetryableFailure (Q6: failed items are final; resume does not retry)", oc2)
	}
	if err2 == nil {
		t.Error("resume: err = nil, want non-nil (same failure as round 1)")
	}
	// Load-bearing assertion: no body Exec calls on round-2. fake2 was
	// programmed but never invoked — items are all committed (Q6 skip).
	if len(fake2.Calls) != 0 {
		t.Errorf("resume re-executed body steps: fake2.Calls = %v, want [] (Q6: failed items must NOT retry on resume)", fake2.Calls)
	}
}

func TestRunMapAsBindingThreaded(t *testing.T) {
	// Verifies that {{ x }} substitution in body actually receives over[K] value.
	// Use 2 items with distinct values; assert each item's dispatched command
	// has the right substitution.
	fake := container.NewFake()
	fake.ProgramExec("./run.sh apple", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("./run.sh banana", container.ExecResult{ExitCode: 0}, nil)
	baseHandle, _ := fake.Create(context.Background(), container.ContainerSpec{Name: "c0"})
	ld := &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{"c0": baseHandle}}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	wf := staticOverWorkflow("fruit", []any{"apple", "banana"}, ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "./run.sh {{ fruit }}", Container: "c0"},
	}, "c0", 2, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState("run-x", "digest", map[string]any{"items": []any{"apple", "banana"}})

	oc, err := runMap(context.Background(), mapNode, "map[0]", wf, rs, ld, lg, blobs, clk, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("got (%q, %v), want (ok, nil)", oc, err)
	}
	// Verify both expected commands were dispatched.
	var sawApple, sawBanana bool
	for _, c := range fake.Calls {
		switch c.Run {
		case "./run.sh apple":
			sawApple = true
		case "./run.sh banana":
			sawBanana = true
		}
	}
	if !sawApple || !sawBanana {
		t.Errorf("substitution failed: sawApple=%v sawBanana=%v; calls=%+v", sawApple, sawBanana, fake.Calls)
	}
}

// ratioInt and ratioStr build *ir.Ratio values for table-driven tests.
// ir.Ratio is a type alias for json.Number (a string). These two helpers
// keep test cases readable without inline conversions.
func ratioInt(i int) *ir.Ratio    { r := ir.Ratio(strconv.Itoa(i)); return &r }
func ratioStr(s string) *ir.Ratio { r := ir.Ratio(s); return &r }

func TestDefaultMinSuccessTable(t *testing.T) {
	// Pins defaultMinSuccess edge cases per slice 3.4 Design Q7 + L13.
	// Total = 5 throughout; varies the MinSuccess input.
	cases := []struct {
		name string
		in   *ir.Ratio
		tot  int
		want int64
	}{
		{"nil", nil, 5, 5},
		{"int 2", ratioInt(2), 5, 2},
		{"int 0 = no-op success", ratioInt(0), 5, 0}, // explicit 0 = "any failure OK"
		{"int -1 conservative", ratioInt(-1), 5, 5},
		{"int > total clamped", ratioInt(10), 5, 5},
		{"fraction 0.6 ceil", ratioStr("0.6"), 5, 3}, // ceil(3.0) = 3
		{"fraction 1.0 = all", ratioStr("1.0"), 5, 5},
		{"fraction 0.0 conservative", ratioStr("0.0"), 5, 5}, // degenerate
		{"fraction > 1 clamped", ratioStr("1.5"), 5, 5},
		{"unparseable conservative", ratioStr("abc"), 5, 5}, // Q7
		{"total 0", nil, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := &ir.Map{MinSuccess: c.in}
			got := defaultMinSuccess(n, c.tot)
			if got != c.want {
				t.Errorf("defaultMinSuccess(MinSuccess=%v, total=%d) = %d, want %d",
					c.in, c.tot, got, c.want)
			}
		})
	}
}

func TestRunMapNonLocalDispatcherErrors(t *testing.T) {
	// Design Q2: runMap requires *LocalDispatcher. Non-local dispatchers
	// (the gate test rigs) error clearly.
	mockDispatcher := &nonLocalMapDispatcher{}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	wf := staticOverWorkflow("x", []any{"a"}, ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "echo {{ x }}", Container: "c0"},
	}, "c0", 1, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState("run-x", "digest", map[string]any{"items": []any{"a"}})

	_, err := runMap(context.Background(), mapNode, "map[0]", wf, rs, mockDispatcher, lg, blobs, clk, nil)
	if err == nil {
		t.Fatal("non-local dispatcher: err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "LocalDispatcher") {
		t.Errorf("err = %v, want mention of \"LocalDispatcher\"", err)
	}
}

type nonLocalMapDispatcher struct{}

func (*nonLocalMapDispatcher) Run(ctx context.Context, intent NodeIntent) (DispatchResult, <-chan container.IOChunk, error) {
	return DispatchResult{}, nil, errors.New("unreachable")
}
