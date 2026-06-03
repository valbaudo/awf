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
	"github.com/valbaudo/awf/template"
)

// Test-wide constants — shared across all runMap tests to remove magic-string
// duplication. The map handler is container-agnostic (per-item handles are
// minted via Backend.Create regardless of name), so a single name suffices.
//
// testRunID is already declared in engine/scope_test.go (same package).
const (
	testMapContainer = "c0"
	testMapPath      = "map[0]"
	testDigest       = "digest"
)

// testClockEpoch is the fixed clock instant fake-clock-backed tests use so
// log Seq/TS ordering is deterministic across the suite.
var testClockEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// mapRig bundles every dependency runMap takes. Returned by newMapRig; tests
// thread the fields into runMap directly. Keeps test bodies focused on the
// scenario instead of plumbing.
type mapRig struct {
	ld    *LocalDispatcher
	fake  *container.Fake
	clk   *clock.Fake
	lg    *state.InMemoryLog
	blobs *state.InMemoryBlobs
}

// newMapRig builds the inputs needed for runMap tests. The variadic programs
// argument pre-loads the fake's exec table (one ProgramExec call per pair).
// Mirrors gateTestDispatcher's setup but uses real types because Design Q2
// requires *LocalDispatcher.
//
// Why pre-create a base handle: LocalDispatcher's Handles map must have an
// entry for the map's declared container name (the dispatch path expects it
// even though runMap layers per-item handles via WithItemHandle).
//
// programs is a variadic of `cmd → result` pairs. Use ok(cmd) / fail(cmd)
// helpers for the common cases; pass `container.ExecResult{...}` directly
// when you need stdout/AWFOutput/etc.
func newMapRig(t *testing.T, programs ...execProgram) *mapRig {
	t.Helper()
	fake := container.NewFake()
	for _, p := range programs {
		fake.ProgramExec(p.cmd, p.res, nil)
	}
	baseHandle, err := fake.Create(context.Background(), container.ContainerSpec{Name: testMapContainer})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	clk := &clock.Fake{T: testClockEpoch}
	return &mapRig{
		ld:    &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{testMapContainer: baseHandle}},
		fake:  fake,
		clk:   clk,
		lg:    state.NewInMemoryLog(clk),
		blobs: state.NewInMemoryBlobs(),
	}
}

// execProgram pairs a command string with its scripted exec result. Used by
// newMapRig's variadic argument. Defined locally to avoid leaking a
// test-helper type into the engine package's public surface.
type execProgram struct {
	cmd string
	res container.ExecResult
}

// ok / fail are convenience constructors for execProgram entries.
func ok(cmd string) execProgram { return execProgram{cmd: cmd, res: container.ExecResult{ExitCode: 0}} }
func fail(cmd string) execProgram {
	return execProgram{cmd: cmd, res: container.ExecResult{ExitCode: 1}}
}

// runOverItems is the standard input map: `{"items": items}`. Used by every
// non-trivial map test (the over expression resolves to input.items).
func runOverItems(items ...any) map[string]any { return map[string]any{"items": items} }

// staticOverWorkflow builds a Workflow with a top-level input and a Map node.
// Body steps reference `{{ <asName> }}` to access the bound item value.
//
// All slice-3.4 unit tests use the same workflow ID + container name (via
// testMapContainer); only asName, body, concurrency, and minSuccess vary
// across tests.
func staticOverWorkflow(asName string, body ir.NodeList, concurrency int, minSuccess *ir.Ratio) *ir.Workflow {
	mapNode := &ir.Map{
		Over:        ir.Expr("{{ input.items }}"),
		As:          asName,
		Container:   testMapContainer,
		Concurrency: concurrency,
		MinSuccess:  minSuccess,
		Body:        body,
	}
	return &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{
			testMapContainer: {Image: "oci://example.com/r@sha256:" + strings.Repeat("0", 64)},
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

// echoStep is the body used by most tests — a single code step that echoes
// the bound item value into stdout via {{ <asName> }} substitution.
func echoStep(asName string, retry *ir.RetryPolicy) ir.NodeList {
	return ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "echo {{ " + asName + " }}", Container: testMapContainer, Retry: retry},
	}
}

func TestRunMapEmptyOver(t *testing.T) {
	rig := newMapRig(t)
	wf := staticOverWorkflow("cve", echoStep("cve", nil), 1, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems())

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("empty over: got (%q, %v), want (ok, nil)", oc, err)
	}
	// No map.item events (no items dispatched).
	events, _ := rig.lg.Fold()
	for _, e := range events {
		if e.Type == EventMapItem {
			t.Errorf("unexpected map.item event with empty over: %+v", e)
		}
	}
}

func TestRunMapSingleItemPasses(t *testing.T) {
	rig := newMapRig(t, ok("echo cve-1"))
	wf := staticOverWorkflow("cve", echoStep("cve", nil), 1, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("cve-1"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("got (%q, %v), want (ok, nil)", oc, err)
	}
	items := rs.LookupMapItems(testMapPath)
	if len(items) != 1 || items[0].Status != ItemPassed {
		t.Errorf("MapItems = %+v, want 1 passed", items)
	}
}

func TestRunMapMultipleItemsAllPass(t *testing.T) {
	rig := newMapRig(t, ok("echo a"), ok("echo b"), ok("echo c"))
	wf := staticOverWorkflow("x", echoStep("x", nil), 2, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("got (%q, %v), want (ok, nil)", oc, err)
	}
	items := rs.LookupMapItems(testMapPath)
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
	//
	// Uses countingBackend (not the standard fake) because we need to count
	// inflight calls + block — newMapRig doesn't apply here.
	fake := container.NewFake()
	var inflight int64
	var maxInflight int64
	release := make(chan struct{})
	cb := &countingBackend{Fake: fake, release: release, inflight: &inflight, maxInflight: &maxInflight}
	baseHandle, _ := cb.Create(context.Background(), container.ContainerSpec{Name: testMapContainer})
	ld := &LocalDispatcher{Backend: cb, Handles: map[string]container.Handle{testMapContainer: baseHandle}}
	clk := &clock.Fake{T: testClockEpoch}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	wf := staticOverWorkflow("x", echoStep("x", nil), 2, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c", "d", "e"))

	done := make(chan struct{})
	go func() {
		_, _ = runMap(context.Background(), mapNode, testMapPath, wf, rs, ld, lg, blobs, clk, nil, nil)
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

func (b *countingBackend) Exec(ctx context.Context, h container.Handle, cmd container.Cmd) (<-chan container.IOChunk, <-chan container.ExecResult, error) {
	cur := atomic.AddInt64(b.inflight, 1)
	for {
		m := atomic.LoadInt64(b.maxInflight)
		if cur <= m || atomic.CompareAndSwapInt64(b.maxInflight, m, cur) {
			break
		}
	}
	<-b.release
	atomic.AddInt64(b.inflight, -1)
	chunks := make(chan container.IOChunk)
	close(chunks)
	result := make(chan container.ExecResult, 1)
	result <- container.ExecResult{ExitCode: 0}
	close(result)
	return chunks, result, nil
}

func TestRunMapMinSuccessTolerates(t *testing.T) {
	// 3 items, min_success: 2; one fails, two pass → map ends ok.
	rig := newMapRig(t, ok("echo a"), fail("echo b"), ok("echo c"))
	minSuccess := ir.Ratio("2")
	wf := staticOverWorkflow("x", echoStep("x", &ir.RetryPolicy{Attempts: 1}), 3, &minSuccess)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("min_success met: got (%q, %v), want (ok, nil)", oc, err)
	}
	pass, fail := countStatuses(rs.LookupMapItems(testMapPath))
	if pass != 2 || fail != 1 {
		t.Errorf("pass=%d fail=%d; want pass=2 fail=1", pass, fail)
	}
}

func TestRunMapMinSuccessFailsBelow(t *testing.T) {
	// 3 items, default min_success (= all); one fails → map fails.
	rig := newMapRig(t, ok("echo a"), fail("echo b"), ok("echo c"))
	wf := staticOverWorkflow("x", echoStep("x", &ir.RetryPolicy{Attempts: 1}), 3, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
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
	rig := newMapRig(t, ok("echo a"), ok("echo c"))
	// Body: if cond=="b" → skip; else → echo.
	body := ir.NodeList{
		&ir.If{
			Cond: ir.Expr(`{{ x == "b" }}`),
			Then: ir.NodeList{&ir.Skip{Reason: "skip middle"}},
			Else: echoStep("x", nil),
		},
	}
	wf := staticOverWorkflow("x", body, 3, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Errorf("skip-in-item: got (%q, %v), want (ok, nil)", oc, err)
	}
	items := rs.LookupMapItems(testMapPath)
	if len(items) != 3 {
		t.Fatalf("MapItems len = %d, want 3", len(items))
	}
	for _, mr := range items {
		if mr.Status != ItemPassed {
			t.Errorf("item N=%d status=%q, want item_passed (skip → ok)", mr.N, mr.Status)
		}
	}
}

// countStatuses tallies ItemPassed / ItemFailed across a MapItems slice.
// Used by min_success tests to assert pass/fail counts.
func countStatuses(items []MapItemRecord) (pass, fail int) {
	for _, mr := range items {
		switch mr.Status {
		case ItemPassed:
			pass++
		case ItemFailed:
			fail++
		}
	}
	return pass, fail
}

// seedRunStartedWithInput appends a run.started event to lg carrying an
// InputRef that points at a freshly-Put input blob. Lets the round-2 Fold
// reconstruct rs.Input via the realistic CLI path (matches cli/run.go's
// pattern). Required because Fold reads Input from RunStartedData.InputRef
// via Blobs.Get (engine/fold.go:86-98) — without this seed, round-2 Fold
// leaves rs.Input nil and `over: "{{ input.items }}"` fails to evaluate.
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
		RunID:          testRunID,
		WorkflowDigest: testDigest,
		InputRef:       inputRef,
	})
	if err != nil {
		t.Fatalf("marshal run.started: %v", err)
	}
	if err := lg.Append(state.Event{Type: EventRunStarted, Data: runStartedData}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
}

// foldFromRig folds the rig's log into a fresh RunState. Used by resume tests
// to simulate the CLI's `awf resume` path (Fold the log → fresh RunState →
// re-enter runMap).
func foldFromRig(t *testing.T, rig *mapRig) *RunState {
	t.Helper()
	events, err := rig.lg.Fold()
	if err != nil {
		t.Fatalf("rig log Fold: %v", err)
	}
	rs, err := Fold(events, rig.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	return rs
}

// bareRig returns a fresh mapRig (no programs) sharing the source rig's log
// + blobs. Used by resume tests: round 2 needs a NEW backend (the CLI
// re-creates containers on resume) but the SAME log + blobs (the
// committed state to fold). Pre-creates the base container handle.
func bareRig(t *testing.T, src *mapRig, programs ...execProgram) *mapRig {
	t.Helper()
	fake := container.NewFake()
	for _, p := range programs {
		fake.ProgramExec(p.cmd, p.res, nil)
	}
	baseHandle, _ := fake.Create(context.Background(), container.ContainerSpec{Name: testMapContainer})
	return &mapRig{
		ld:    &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{testMapContainer: baseHandle}},
		fake:  fake,
		clk:   src.clk,
		lg:    src.lg,
		blobs: src.blobs,
	}
}

func TestRunMapResumeReplaysCommittedItems(t *testing.T) {
	// Per spec §8 + Design Q6: committed items are REPLAYED on resume,
	// not re-executed. Round 1 commits all 3 as item_passed; round 2
	// resumes against a BARE fake (no programmed Exec entries). If
	// runMap correctly skips committed items, no Exec calls happen on
	// round 2 → resume completes ok. If it incorrectly re-executes,
	// the bare fake errors them → fake.Calls is non-empty.

	rig1 := newMapRig(t, ok("echo a"), ok("echo b"), ok("echo c"))
	input := runOverItems("a", "b", "c")
	seedRunStartedWithInput(t, rig1.lg, rig1.blobs, input)
	wf := staticOverWorkflow("x", echoStep("x", nil), 3, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs1 := NewRunState(testRunID, testDigest, input)

	oc1, err1 := runMap(context.Background(), mapNode, testMapPath, wf, rs1, rig1.ld, rig1.lg, rig1.blobs, rig1.clk, nil, nil)
	if oc1 != OutcomeOK || err1 != nil {
		t.Fatalf("round-1: got (%q, %v), want (ok, nil)", oc1, err1)
	}
	if pre := rs1.LookupMapItems(testMapPath); len(pre) != 3 {
		t.Fatalf("pre-resume MapItems len = %d, want 3", len(pre))
	}

	// Round 2: BARE fake (no programs). Any body re-execution would error.
	rig2 := bareRig(t, rig1) // no programs
	rs2 := foldFromRig(t, rig2)
	if rs2.Input == nil {
		t.Fatal("Fold did not reconstruct rs2.Input from InputRef")
	}

	oc2, err2 := runMap(context.Background(), mapNode, testMapPath, wf, rs2, rig2.ld, rig2.lg, rig2.blobs, rig2.clk, nil, nil)
	if oc2 != OutcomeOK || err2 != nil {
		t.Fatalf("resume: got (%q, %v), want (ok, nil) — committed items must replay, not re-execute", oc2, err2)
	}
	// Load-bearing: round-2 fake received ZERO Exec calls.
	if len(rig2.fake.Calls) != 0 {
		t.Errorf("resume re-executed body steps: fake.Calls = %v, want []", rig2.fake.Calls)
	}

	post := rs2.LookupMapItems(testMapPath)
	if len(post) != 3 {
		t.Fatalf("post-resume MapItems len = %d, want 3", len(post))
	}
	for _, mr := range post {
		if mr.Status != ItemPassed {
			t.Errorf("post-resume N=%d status=%q, want item_passed (Q6: committed status preserved)", mr.N, mr.Status)
		}
	}
}

func TestRunMapResumeFailedItemsStayFailed(t *testing.T) {
	// Per Design Q6: failed items are FINAL across resume — they are
	// committed as item_failed and resume SKIPS them.
	//
	// Round 1: 2 pass + 1 fail with min_success: 3 → map fails.
	// Round 2: fresh fake with ALL 3 programmed to succeed. If item-1
	// got a second chance, map would return ok. Q6 says NO retry — resume
	// returns the SAME failure with no body re-execution.
	//
	// CALLER NOTE: exercises runMap directly. Via the CLI, `awf resume`
	// refuses runs with terminal run.finished (slice 2.6); failed runs
	// aren't user-resumable through that path. This test pins the engine-
	// level Q6 contract for callers that bypass the refusal.

	rig1 := newMapRig(t, ok("echo a"), fail("echo b"), ok("echo c"))
	input := runOverItems("a", "b", "c")
	seedRunStartedWithInput(t, rig1.lg, rig1.blobs, input)
	minSuccess := ir.Ratio("3")
	wf := staticOverWorkflow("x", echoStep("x", &ir.RetryPolicy{Attempts: 1}), 3, &minSuccess)
	mapNode := wf.Graph[0].(*ir.Map)
	rs1 := NewRunState(testRunID, testDigest, input)

	oc1, err1 := runMap(context.Background(), mapNode, testMapPath, wf, rs1, rig1.ld, rig1.lg, rig1.blobs, rig1.clk, nil, nil)
	if oc1 != OutcomeRetryableFailure {
		t.Fatalf("round-1: outcome = %q, want OutcomeRetryableFailure", oc1)
	}
	if err1 == nil {
		t.Fatal("round-1: err = nil, want non-nil")
	}
	pass, fail := countStatuses(rs1.LookupMapItems(testMapPath))
	if pass != 2 || fail != 1 {
		t.Fatalf("round-1: pre-resume pass=%d fail=%d, want 2/1", pass, fail)
	}

	// Round 2: fresh fake with ALL 3 programmed to succeed (would-pass-on-retry).
	rig2 := bareRig(t, rig1, ok("echo a"), ok("echo b"), ok("echo c"))
	rs2 := foldFromRig(t, rig2)

	oc2, err2 := runMap(context.Background(), mapNode, testMapPath, wf, rs2, rig2.ld, rig2.lg, rig2.blobs, rig2.clk, nil, nil)
	if oc2 != OutcomeRetryableFailure {
		t.Errorf("resume: outcome = %q, want OutcomeRetryableFailure (Q6: failed items final)", oc2)
	}
	if err2 == nil {
		t.Error("resume: err = nil, want non-nil")
	}
	if len(rig2.fake.Calls) != 0 {
		t.Errorf("resume re-executed body: fake.Calls = %v, want [] (Q6)", rig2.fake.Calls)
	}
}

func TestRunMapAsBindingThreaded(t *testing.T) {
	// Verifies {{ x }} substitution in body actually receives over[K] value.
	// Uses 2 items with distinct values; asserts each item's dispatched
	// command had the right substitution.
	rig := newMapRig(t, ok("./run.sh apple"), ok("./run.sh banana"))
	body := ir.NodeList{
		&ir.CodeStep{ID: "echo", Run: "./run.sh {{ fruit }}", Container: testMapContainer},
	}
	wf := staticOverWorkflow("fruit", body, 2, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("apple", "banana"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("got (%q, %v), want (ok, nil)", oc, err)
	}
	var sawApple, sawBanana bool
	for _, c := range rig.fake.Calls {
		switch c.Run {
		case "./run.sh apple":
			sawApple = true
		case "./run.sh banana":
			sawBanana = true
		}
	}
	if !sawApple || !sawBanana {
		t.Errorf("substitution failed: sawApple=%v sawBanana=%v; calls=%+v", sawApple, sawBanana, rig.fake.Calls)
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
	clk := &clock.Fake{T: testClockEpoch}
	lg, blobs := state.NewInMemoryLog(clk), state.NewInMemoryBlobs()
	wf := staticOverWorkflow("x", echoStep("x", nil), 1, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("a"))

	_, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, mockDispatcher, lg, blobs, clk, nil, nil)
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

// findingSchema is the output_schema for the aggregate-chain test's body step:
// a single required string `finding`.
var findingSchema = &ir.JSONSchema{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"finding"},
	"properties":           map[string]any{"finding": map[string]any{"type": "string"}},
}

func TestRunMapAggregateChainResume(t *testing.T) {
	// Engine-level chain + resume for map output aggregation (Approach A,
	// design §1.4/§1.6). Map A runs a body step `scan` producing a typed
	// `finding` per item over 3 items; item-1 ("b") fails (compaction). After
	// round 1, the passed items have committed Completed entries. After resume,
	// resolving the aggregate `step.scan` from a site OUTSIDE map A yields the
	// same index-ordered []any of the passed items' outputs — proving the
	// aggregate replays identically post-resume.

	// Body: one code step with an output_schema; each item runs a distinct
	// command via {{ x }} substitution so the fake can return distinct outputs.
	body := ir.NodeList{
		&ir.CodeStep{ID: "scan", Run: "./scan {{ x }}", Container: testMapContainer,
			OutputSchema: findingSchema, Retry: &ir.RetryPolicy{Attempts: 1}},
	}
	minSuccess := ir.Ratio("2") // tolerate item-1's failure
	// Concurrency 1: the body step has an output_schema, so each item commits a
	// typed-output blob via Commit→Blobs.Put. InMemoryBlobs is explicitly NOT
	// safe for concurrent Put (state/fake.go) — the production FSBlobs is (atomic
	// temp-file + rename), but the test fake is single-writer. Serializing here
	// keeps the fake honest without weakening the aggregate semantics under test
	// (aggregation is concurrency-independent). The other concurrent map tests
	// dodge this only because echoStep has no output_schema (no blob Put).
	mkWF := func() *ir.Workflow { return staticOverWorkflow("x", body, 1, &minSuccess) }

	// Round 1: items a, c pass with typed findings; b fails (exit 1).
	rig1 := newMapRig(t,
		execProgram{cmd: "./scan a", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"finding":"FA"}`)}},
		execProgram{cmd: "./scan b", res: container.ExecResult{ExitCode: 1}},
		execProgram{cmd: "./scan c", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"finding":"FC"}`)}},
	)
	input := runOverItems("a", "b", "c")
	seedRunStartedWithInput(t, rig1.lg, rig1.blobs, input)
	wf1 := mkWF()
	mapNode := wf1.Graph[0].(*ir.Map)
	rs1 := NewRunState(testRunID, testDigest, input)

	oc1, err1 := runMap(context.Background(), mapNode, testMapPath, wf1, rs1, rig1.ld, rig1.lg, rig1.blobs, rig1.clk, nil, nil)
	if oc1 != OutcomeOK || err1 != nil {
		t.Fatalf("round-1: got (%q, %v), want (ok, nil) — min_success 2 tolerates one failure", oc1, err1)
	}
	// The passed items (0, 2) committed their scan step; the failed item (1) did not.
	for _, k := range []int{0, 2} {
		if _, ok := rs1.LookupCompleted(ItemPath(testMapPath, k) + ".scan"); !ok {
			t.Fatalf("round-1: Completed[%s.scan] missing for passed item", ItemPath(testMapPath, k))
		}
	}
	if _, ok := rs1.LookupCompleted(ItemPath(testMapPath, 1) + ".scan"); ok {
		t.Fatalf("round-1: Completed[%s.scan] present for failed item — expected absent (compaction)", ItemPath(testMapPath, 1))
	}

	// Resume: fresh RunState folded from the committed log, BARE fake (no
	// programs — committed items must replay, not re-execute).
	rig2 := bareRig(t, rig1)
	rs2 := foldFromRig(t, rig2)
	wf2 := mkWF()

	// Aggregate resolution from a site OUTSIDE map[0] (a sibling map's over:).
	sc := NewScope(rs2, wf2, "map[1].over")
	got, err := sc.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "step"}, {Ident: "scan"}}})
	if err != nil {
		t.Fatalf("resume aggregate resolve: %v", err)
	}
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("resume aggregate type = %T, want []any", got)
	}
	// Length == committed (passed) count; values match A's outputs, index-ordered.
	if len(arr) != 2 {
		t.Fatalf("resume aggregate len = %d, want 2 (passed items, failed item compacted)", len(arr))
	}
	want := []string{"FA", "FC"}
	for i, w := range want {
		m, ok := arr[i].(map[string]any)
		if !ok {
			t.Fatalf("resume aggregate[%d] = %#v, want map[string]any", i, arr[i])
		}
		if m["finding"] != w {
			t.Errorf("resume aggregate[%d].finding = %v, want %q", i, m["finding"], w)
		}
	}

	// Field projection (step.scan.finding) likewise compacts + orders.
	projV, err := sc.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "step"}, {Ident: "scan"}, {Ident: "finding"}}})
	if err != nil {
		t.Fatalf("resume field aggregate resolve: %v", err)
	}
	proj, ok := projV.([]any)
	if !ok || len(proj) != 2 || proj[0] != "FA" || proj[1] != "FC" {
		t.Fatalf("resume field aggregate = %#v, want []any{\"FA\",\"FC\"}", projV)
	}
}
