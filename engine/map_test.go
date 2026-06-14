package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
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

// seedCommittedMapItem appends a committed map.item event to lg (the durable
// channel resume folds). Used by the prune-denominator tests to pre-seed pruned
// / passed items WITHOUT the Task 6/7 controller wiring: runMap's resume
// reconciliation reads these back via committed[N] and fills `statuses` from
// them, so the tally runs over the durable record (replayed, not re-decided).
func seedCommittedMapItem(t *testing.T, lg state.Log, mapPath string, n int, status string) {
	t.Helper()
	data, err := json.Marshal(MapItemData{N: n, Status: status})
	if err != nil {
		t.Fatalf("marshal map.item N=%d: %v", n, err)
	}
	if err := lg.Append(state.Event{Type: EventMapItem, Path: mapPath, Data: data}); err != nil {
		t.Fatalf("append map.item N=%d: %v", n, err)
	}
}

func TestRunMapPrunedExcludedFromMinSuccess(t *testing.T) {
	// SP5 Task 4: pruned items are removed from BOTH the numerator AND the
	// min_success denominator. 4 items, 2 pruned + 2 passed, min_success unset
	// (= all). With pruned in the denominator the map would need 4 passes and
	// fail ("2 passed"); excluding them, "all" means the 2 NON-pruned, both of
	// which passed → OutcomeOK.
	//
	// Drive via the resume path: pre-seed the durable map.item record, fold,
	// then run against a BARE fake. All items are committed, so the dispatch
	// loop skips them and the tally runs over [passed, passed, pruned, pruned].
	rig1 := newMapRig(t)
	input := runOverItems("a", "b", "c", "d")
	seedRunStartedWithInput(t, rig1.lg, rig1.blobs, input)
	seedCommittedMapItem(t, rig1.lg, testMapPath, 0, ItemPassed)
	seedCommittedMapItem(t, rig1.lg, testMapPath, 1, ItemPruned)
	seedCommittedMapItem(t, rig1.lg, testMapPath, 2, ItemPruned)
	seedCommittedMapItem(t, rig1.lg, testMapPath, 3, ItemPassed)

	wf := staticOverWorkflow("x", echoStep("x", nil), 4, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := foldFromRig(t, rig1)

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig1.ld, rig1.lg, rig1.blobs, rig1.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("pruned excluded from min_success: got (%q, %v), want (ok, nil) — pruned items must leave the denominator", oc, err)
	}
	if len(rig1.fake.Calls) != 0 {
		t.Errorf("committed items re-executed: fake.Calls = %v, want []", rig1.fake.Calls)
	}
}

func TestRunMapAllPrunedIsOK(t *testing.T) {
	// SP5 Task 4 edge case: if EVERY item is pruned the effective denominator
	// is 0; defaultMinSuccess(n, 0) = 0 and pass(0) >= 0 → OutcomeOK. An
	// entirely-pruned frontier (e.g. stop_when fired immediately) is a success,
	// not a failure — nothing was expected to survive.
	rig1 := newMapRig(t)
	input := runOverItems("a", "b", "c")
	seedRunStartedWithInput(t, rig1.lg, rig1.blobs, input)
	seedCommittedMapItem(t, rig1.lg, testMapPath, 0, ItemPruned)
	seedCommittedMapItem(t, rig1.lg, testMapPath, 1, ItemPruned)
	seedCommittedMapItem(t, rig1.lg, testMapPath, 2, ItemPruned)

	wf := staticOverWorkflow("x", echoStep("x", nil), 3, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := foldFromRig(t, rig1)

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig1.ld, rig1.lg, rig1.blobs, rig1.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("all-pruned: got (%q, %v), want (ok, nil)", oc, err)
	}
}

// scoreSchema is the output_schema for prune body steps: a single numeric
// `score` field (the value the prune frontier reads per item).
var scoreSchema = &ir.JSONSchema{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"score"},
	"properties":           map[string]any{"score": map[string]any{"type": "number"}},
}

// pruneBody is the standard prune body: a single code step `hyp` that runs
// `./hyp {{ x }}` and declares the numeric `score` output. Concurrency-1 tests
// must use this with retry attempts 1 (the InMemoryBlobs Put is single-writer).
func pruneBody(retry *ir.RetryPolicy) ir.NodeList {
	return ir.NodeList{
		&ir.CodeStep{ID: "hyp", Run: "./hyp {{ x }}", Container: testMapContainer,
			OutputSchema: scoreSchema, Retry: retry},
	}
}

// pruneWorkflow builds a Workflow whose single Map carries a prune: clause.
func pruneWorkflow(concurrency int, prune *ir.Prune) *ir.Workflow {
	wf := staticOverWorkflow("x", pruneBody(&ir.RetryPolicy{Attempts: 1}), concurrency, nil)
	wf.Graph[0].(*ir.Map).Prune = prune
	return wf
}

// scoreProg builds an execProgram returning a typed {score: v} for `./hyp <item>`.
func scoreProg(item string, v float64) execProgram {
	raw, _ := json.Marshal(map[string]any{"score": v})
	return execProgram{cmd: "./hyp " + item, res: container.ExecResult{ExitCode: 0, AWFOutput: raw}}
}

// statusByN returns N → Status for a MapItems slice (test convenience).
func statusByN(items []MapItemRecord) map[int]string {
	out := map[int]string{}
	for _, mr := range items {
		out[mr.N] = mr.Status
	}
	return out
}

func TestRunMapKeepTopK(t *testing.T) {
	// SP5 Task 7: keep: top(2) over 4 items with scores [0.1, 0.9, 0.5, 0.7].
	// The two highest scorers (indices 1, 3) survive (item_passed); the two
	// lowest (indices 0, 2) are pruned (item_pruned). A pruned item is NEITHER
	// a pass NOR a failure, so the map returns OutcomeOK with no error.
	rig := newMapRig(t,
		scoreProg("a", 0.1), scoreProg("b", 0.9), scoreProg("c", 0.5), scoreProg("d", 0.7))
	wf := pruneWorkflow(4, &ir.Prune{Score: "score", Keep: &ir.PruneKeep{K: 2}})
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c", "d"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("keep top(2): got (%q, %v), want (ok, nil)", oc, err)
	}
	got := statusByN(rs.LookupMapItems(testMapPath))
	want := map[int]string{0: ItemPruned, 1: ItemPassed, 2: ItemPruned, 3: ItemPassed}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("item N=%d status=%q, want %q", n, got[n], w)
		}
	}
	// No node.failed / errors: prune is not a failure path.
	events, _ := rig.lg.Fold()
	for _, e := range events {
		if e.Type == EventNodeFailed {
			t.Errorf("unexpected node.failed event in a pruned map: %+v", e)
		}
	}
}

func TestRunMapStopWhen(t *testing.T) {
	// SP5 Task 7: stop_when "best.score >= 0.9" over 4 items that all score 0.95,
	// concurrency 1. Under a single slot the FIRST item to run commits 0.95, which
	// trips stop_when; every other item is then pruned (queued-loser short-circuit
	// — body never runs) before it ever acquires the slot. WHICH index runs first
	// is scheduler-dependent, so the test asserts the order-INDEPENDENT invariant
	// stop_when guarantees: exactly ONE item passes and the rest are pruned. The
	// pruned items' bodies never executed (exactly one Exec call). Map → ok.
	rig := newMapRig(t, scoreProg("a", 0.95), scoreProg("b", 0.95), scoreProg("c", 0.95), scoreProg("d", 0.95))
	wf := pruneWorkflow(1, &ir.Prune{Score: "score", StopWhen: "{{ best.score >= 0.9 }}"})
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c", "d"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("stop_when: got (%q, %v), want (ok, nil)", oc, err)
	}
	var pass, pruned int
	for _, mr := range rs.LookupMapItems(testMapPath) {
		switch mr.Status {
		case ItemPassed:
			pass++
		case ItemPruned:
			pruned++
		default:
			t.Errorf("item N=%d unexpected status %q", mr.N, mr.Status)
		}
	}
	if pass != 1 || pruned != 3 {
		t.Errorf("stop_when tally: pass=%d pruned=%d, want pass=1 pruned=3 (the trigger passes, the rest are pruned)", pass, pruned)
	}
	// Exactly one body ran: the pruned items were short-circuited before dispatch.
	if len(rig.fake.Calls) != 1 {
		t.Errorf("pruned item bodies executed: fake.Calls = %v, want exactly 1 (the trigger)", rig.fake.Calls)
	}
}

func TestRunMapPrunedDoesNotTripTry(t *testing.T) {
	// SP5 Task 7: a pruned item is NOT a failure, so a prune map inside a
	// try.do must NOT cause the try to enter catch. The catch would commit a
	// sentinel code step; assert the sentinel never ran and the run is ok.
	rig := newMapRig(t,
		scoreProg("a", 0.1), scoreProg("b", 0.9), scoreProg("c", 0.5), scoreProg("d", 0.7))
	mapNode := &ir.Map{
		Over:        ir.Expr("{{ input.items }}"),
		As:          "x",
		Container:   testMapContainer,
		Concurrency: 4,
		Body:        pruneBody(&ir.RetryPolicy{Attempts: 1}),
		Prune:       &ir.Prune{Score: "score", Keep: &ir.PruneKeep{K: 2}},
	}
	tryNode := &ir.Try{
		Do:    ir.NodeList{mapNode},
		Catch: ir.NodeList{&ir.CodeStep{ID: "sentinel", Run: "./sentinel", Container: testMapContainer}},
	}
	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{
			testMapContainer: {Image: "oci://example.com/r@sha256:" + strings.Repeat("0", 64)},
		},
		Input: &ir.JSONSchema{
			"type":       "object",
			"properties": map[string]any{"items": map[string]any{"type": "array"}},
		},
		Graph: ir.NodeList{tryNode},
	}
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c", "d"))

	oc, err := interpNodes(context.Background(), wf.Graph, "", interpreterContext{
		wf: wf, runstate: rs, dispatcher: rig.ld, log: rig.lg, blobs: rig.blobs, clk: rig.clk,
	})
	if oc != OutcomeOK || err != nil {
		t.Fatalf("prune-in-try: got (%q, %v), want (ok, nil) — pruned items must not trip catch", oc, err)
	}
	for _, c := range rig.fake.Calls {
		if strings.Contains(c.Run, "./sentinel") {
			t.Errorf("catch ran on a pruned map: fake.Calls = %v, want ./sentinel absent", rig.fake.Calls)
		}
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

// okSchema is the output_schema for the reduce-quorum body step: a single
// required boolean `ok` that quorum counts via reduce.over.
var okSchema = &ir.JSONSchema{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"ok"},
	"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
}

func TestRunMapQuorumReducePrefersReducedOutput(t *testing.T) {
	// Task 11: a map declaring reduce: {quorum: 2, over: ok} over 3 items
	// (2 true) commits a node.completed at the MAP path (map[0]) with the
	// reduced {passed:true,...} output and runMap returns ok. A downstream
	// step.<bodyId> ref from OUTSIDE the map then resolves to the REDUCED
	// output (not the per-item array).
	body := ir.NodeList{
		&ir.CodeStep{ID: "vote", Run: "./vote {{ x }}", Container: testMapContainer,
			OutputSchema: okSchema, Retry: &ir.RetryPolicy{Attempts: 1}},
	}
	q := ir.Ratio("2")
	wf := staticOverWorkflow("x", body, 1, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	mapNode.Reduce = &ir.Reduce{Quorum: &q, Over: "ok"}

	rig := newMapRig(t,
		execProgram{cmd: "./vote a", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":true}`)}},
		execProgram{cmd: "./vote b", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":true}`)}},
		execProgram{cmd: "./vote c", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":false}`)}},
	)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("quorum reduce: got (%q, %v), want (ok, nil)", oc, err)
	}

	// The map node itself committed the reduced output at the bare map path.
	nr, ok := rs.LookupCompleted(testMapPath)
	if !ok {
		t.Fatalf("no NodeResult committed at map path %q (reduce must commit there)", testMapPath)
	}
	if nr.Outputs["passed"] != true {
		t.Errorf("reduced passed = %v, want true", nr.Outputs["passed"])
	}
	if nr.Outputs["votes"] != 3 || nr.Outputs["agree"] != 2 {
		t.Errorf("reduced {votes,agree} = {%v,%v}, want {3,2}", nr.Outputs["votes"], nr.Outputs["agree"])
	}

	// A downstream step.vote.passed ref (from outside the map) lifts the REDUCED
	// output, not the per-item array.
	sc := NewScope(rs, wf, "after_map")
	got, err := sc.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "step"}, {Ident: "vote"}, {Ident: "passed"}}})
	if err != nil {
		t.Fatalf("resolve step.vote.passed: %v", err)
	}
	if got != true {
		t.Errorf("step.vote.passed = %v (%T), want true (the reduced output)", got, got)
	}
}

func TestRunMapQuorumReduceNotMetIsRetryable(t *testing.T) {
	// Task 11: a quorum that is not met returns retryable_failure and commits
	// NO node.completed at the map path (mirrors min_success not met).
	body := ir.NodeList{
		&ir.CodeStep{ID: "vote", Run: "./vote {{ x }}", Container: testMapContainer,
			OutputSchema: okSchema, Retry: &ir.RetryPolicy{Attempts: 1}},
	}
	q := ir.Ratio("3") // need all 3 true, only 2 are
	wf := staticOverWorkflow("x", body, 1, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	mapNode.Reduce = &ir.Reduce{Quorum: &q, Over: "ok"}

	rig := newMapRig(t,
		execProgram{cmd: "./vote a", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":true}`)}},
		execProgram{cmd: "./vote b", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":true}`)}},
		execProgram{cmd: "./vote c", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":false}`)}},
	)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeRetryableFailure {
		t.Fatalf("quorum not met: outcome = %q (err=%v), want retryable_failure", oc, err)
	}
	if err == nil {
		t.Fatal("quorum not met: err = nil, want non-nil")
	}
	if _, ok := rs.LookupCompleted(testMapPath); ok {
		t.Errorf("a not-met quorum must NOT commit a NodeResult at the map path %q", testMapPath)
	}
}

func TestRunMapQuorumReduceThresholdIsCohortWhenBranchCrashes(t *testing.T) {
	// Task 11 regression (review): the quorum threshold is measured against the
	// fan-out COHORT, not the survivor count. Map over 3 items where 1 branch
	// crashes mechanically (item "c" exits nonzero → ItemFailed → absent from
	// collectReduceBranches) and the 2 survivors agree. quorum: 3 (unanimous over
	// the cohort) must FAIL — only 2 of the 3-item cohort agree. The old code
	// measured k against len(branches)=2 and the int-cap silently lowered need to
	// 2, passing on the survivors.
	body := ir.NodeList{
		&ir.CodeStep{ID: "vote", Run: "./vote {{ x }}", Container: testMapContainer,
			OutputSchema: okSchema, Retry: &ir.RetryPolicy{Attempts: 1}},
	}
	q := ir.Ratio("3") // unanimous over the 3-item cohort
	wf := staticOverWorkflow("x", body, 1, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	mapNode.Reduce = &ir.Reduce{Quorum: &q, Over: "ok"}

	rig := newMapRig(t,
		execProgram{cmd: "./vote a", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":true}`)}},
		execProgram{cmd: "./vote b", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":true}`)}},
		execProgram{cmd: "./vote c", res: container.ExecResult{ExitCode: 1}}, // crashes → ItemFailed, absent branch
	)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeRetryableFailure {
		t.Fatalf("unanimous quorum over a cohort with one crashed branch: outcome = %q (err=%v), want retryable_failure", oc, err)
	}
	if err == nil {
		t.Fatal("a not-met quorum (cohort threshold) must return a non-nil error")
	}
	if _, ok := rs.LookupCompleted(testMapPath); ok {
		t.Errorf("a not-met quorum must NOT commit a NodeResult at the map path %q", testMapPath)
	}
}

func TestRunMapRunReduceReusesPreProvisionedContainer(t *testing.T) {
	// Regression: a run-reduce whose container is a PRE-DECLARED one (already
	// brought up + present in ld.Handles, like every declared container at run
	// start) must REUSE that handle — runMapReduce must NOT Create+Destroy it. The
	// old code did (it "mirrored" dispatchItem's per-ITEM ephemeral containers),
	// which for a compose project tears the WHOLE project down when the reduce
	// returns — destroying a lab that LATER steps still use. That was the slice5
	// item4-reduce → item5 "Exec: unknown handle <lab>" failure (slice2 never hit
	// it because nothing ran after its reduce).
	rowSchema := &ir.JSONSchema{
		"type": "object", "additionalProperties": false,
		"required": []any{"k"}, "properties": map[string]any{"k": map[string]any{"type": "string"}},
	}
	reduceSchema := &ir.JSONSchema{
		"type": "object", "additionalProperties": false,
		"required": []any{"csv_rows"}, "properties": map[string]any{"csv_rows": map[string]any{"type": "integer"}},
	}
	body := ir.NodeList{
		&ir.CodeStep{ID: "scan", Run: "./scan {{ x }}", Container: testMapContainer,
			OutputSchema: rowSchema, OutputFiles: ir.OutputFiles{{Name: "row", Path: "/out/row.csv"}},
			Retry: &ir.RetryPolicy{Attempts: 1}},
	}
	wf := staticOverWorkflow("x", body, 1, nil)
	wf.Containers["agg"] = ir.Container{Image: "oci://example.com/r@sha256:" + strings.Repeat("3", 64)}
	mapNode := wf.Graph[0].(*ir.Map)
	mapNode.Reduce = &ir.Reduce{
		Run: "./merge.sh", Container: "agg", OutputSchema: reduceSchema,
		OutputFiles: ir.OutputFiles{{Name: "csv", Path: "/out/versions.csv"}},
	}

	rig := newMapRig(t, execProgram{cmd: "./scan a", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"k":"a"}`)}})
	rig.fake.ProgramExecWithFiles("./merge.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"csv_rows":1}`)},
		nil, map[string][]byte{"/out/versions.csv": []byte("merged")})

	// Pre-provision the reduce container 'agg' in ld.Handles (as a real run does
	// for EVERY declared container at run start).
	aggH, err := rig.fake.Create(context.Background(), container.ContainerSpec{Name: "agg"})
	if err != nil {
		t.Fatalf("seed agg: %v", err)
	}
	rig.ld.Handles["agg"] = aggH
	countAggCreates := func() int {
		n := 0
		for _, s := range rig.fake.CreateSpecs {
			if s.Name == "agg" {
				n++
			}
		}
		return n
	}
	before := countAggCreates() // 1 (the seed above)

	rs := NewRunState(testRunID, testDigest, runOverItems("a"))
	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("runMap with a run-reduce into a pre-provisioned container: (%q, %v), want (ok, nil)", oc, err)
	}
	if after := countAggCreates(); after != before {
		t.Errorf("the reduce re-Created the PRE-PROVISIONED container 'agg' (%d→%d Backend.Create calls); the reducer must REUSE ld.Handles[agg], not Create+Destroy it — Create+Destroy of a shared compose project tears down a lab that later steps still need", before, after)
	}
	if _, ok := rs.LookupCompleted(testMapPath); !ok {
		t.Errorf("the reduce did not commit a result at the map path %q (it must still RUN, just in the existing handle)", testMapPath)
	}
}

func TestRunMapRunReduceServiceOverrideReusesBarePreProvisionedContainer(t *testing.T) {
	rowSchema := &ir.JSONSchema{
		"type": "object", "additionalProperties": false,
		"required": []any{"k"}, "properties": map[string]any{"k": map[string]any{"type": "string"}},
	}
	reduceSchema := &ir.JSONSchema{
		"type": "object", "additionalProperties": false,
		"required": []any{"csv_rows"}, "properties": map[string]any{"csv_rows": map[string]any{"type": "integer"}},
	}
	body := ir.NodeList{
		&ir.CodeStep{ID: "scan", Run: "./scan {{ x }}", Container: testMapContainer,
			OutputSchema: rowSchema, Retry: &ir.RetryPolicy{Attempts: 1}},
	}
	wf := staticOverWorkflow("x", body, 1, nil)
	wf.Containers["lab"] = ir.Container{Image: "oci://example.com/lab@sha256:" + strings.Repeat("4", 64)}
	mapNode := wf.Graph[0].(*ir.Map)
	mapNode.Reduce = &ir.Reduce{
		Run: "./merge.sh", Container: "lab:api", OutputSchema: reduceSchema,
		OutputFiles: ir.OutputFiles{{Name: "csv", Path: "/out/versions.csv"}},
	}

	rig := newMapRig(t, execProgram{cmd: "./scan a", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"k":"a"}`)}})
	rig.fake.ProgramExecWithFiles("./merge.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"csv_rows":1}`)},
		nil, map[string][]byte{"/out/versions.csv": []byte("merged")})

	labH, err := rig.fake.Create(context.Background(), container.ContainerSpec{Name: "lab", Service: "web"})
	if err != nil {
		t.Fatalf("seed lab: %v", err)
	}
	rig.ld.Handles["lab"] = labH

	rs := NewRunState(testRunID, testDigest, runOverItems("a"))
	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("runMap with service override reducer: (%q, %v), want (ok, nil)", oc, err)
	}
	for _, spec := range rig.fake.CreateSpecs {
		if spec.Name == "lab:api" {
			t.Fatalf("run reducer created a full-ref container %q; want reuse of bare handle \"lab\"", spec.Name)
		}
	}
	var reduceHandle *container.Handle
	for i, call := range rig.fake.Calls {
		if call.Run == "./merge.sh" {
			reduceHandle = &rig.fake.ExecHandles[i]
			break
		}
	}
	if reduceHandle == nil {
		t.Fatal("reducer command ./merge.sh was not executed")
	}
	if reduceHandle.ID != labH.ID {
		t.Errorf("reducer handle ID = %q, want pre-provisioned lab handle %q", reduceHandle.ID, labH.ID)
	}
	if reduceHandle.Service != "api" {
		t.Errorf("reducer handle service = %q, want api override", reduceHandle.Service)
	}
}

func TestRunMapRunReduceResumeReplaysWithoutBootingContainer(t *testing.T) {
	// Task 11 regression (review): on a pure replay of an already-committed
	// run: reducer, runMap must NOT Create (and tear down) the reducer container.
	// A committed reduce replays; booting infra for it turns a should-be-pure
	// replay into work that can FAIL the resume (e.g. an image no longer
	// pullable), violating "infra is rebuilt only for the uncommitted frontier."
	rowSchema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"k"},
		"properties":           map[string]any{"k": map[string]any{"type": "string"}},
	}
	body := ir.NodeList{
		&ir.CodeStep{ID: "scan", Run: "./scan {{ x }}", Container: testMapContainer,
			OutputSchema: rowSchema,
			OutputFiles:  ir.OutputFiles{{Name: "row", Path: "/out/row.csv"}},
			Retry:        &ir.RetryPolicy{Attempts: 1}},
	}
	reduceSchema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"csv_rows"},
		"properties":           map[string]any{"csv_rows": map[string]any{"type": "integer"}},
	}
	wf := staticOverWorkflow("x", body, 1, nil)
	wf.Containers["agg"] = ir.Container{Image: "oci://example.com/r@sha256:" + strings.Repeat("3", 64)}
	mapNode := wf.Graph[0].(*ir.Map)
	mapNode.Reduce = &ir.Reduce{
		Run:          "./merge.sh",
		Container:    "agg",
		OutputSchema: reduceSchema,
		OutputFiles:  ir.OutputFiles{{Name: "csv", Path: "/out/versions.csv"}},
	}

	rig := newMapRig(t)
	// Pre-seed the committed map items + the committed reduced result at the map
	// path, so this runMap call is a pure replay. The merge.sh command is NOT
	// programmed — if the reducer re-ran (or its container booted) the test would
	// surface it (re-exec → unprogrammed command error / Create on CreateSpecs).
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b"))
	rs.RecordMapItem(testMapPath, MapItemRecord{N: 0, ItemValue: "a", Status: ItemPassed})
	rs.RecordMapItem(testMapPath, MapItemRecord{N: 1, ItemValue: "b", Status: ItemPassed})
	rs.RecordCompleted(testMapPath, NodeResult{Outcome: OutcomeOK, Outputs: map[string]any{"csv_rows": float64(2)}})

	createsBefore := len(rig.fake.CreateSpecs)
	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("resume replay: got (%q, %v), want (ok, nil)", oc, err)
	}
	// No reducer container was Created on replay (the committed-item skip means
	// no per-item Create either; the only Create is newMapRig's base handle,
	// which happened before this call).
	for _, spec := range rig.fake.CreateSpecs[createsBefore:] {
		if spec.Name == "agg" {
			t.Errorf("resume replay booted the reducer container %q — a committed reduce must replay, not re-provision infra", spec.Name)
		}
	}
	if got := len(rig.fake.CreateSpecs) - createsBefore; got != 0 {
		t.Errorf("resume replay made %d Create call(s); want 0 (pure replay rebuilds no infra)", got)
	}
}

func TestRunMapNoReduceStillAggregatesArray(t *testing.T) {
	// Task 11 regression: without a reduce:, step.<bodyId> still lifts the
	// per-item array (existing aggregation behavior unbroken).
	body := ir.NodeList{
		&ir.CodeStep{ID: "vote", Run: "./vote {{ x }}", Container: testMapContainer,
			OutputSchema: okSchema, Retry: &ir.RetryPolicy{Attempts: 1}},
	}
	wf := staticOverWorkflow("x", body, 1, nil) // no Reduce
	mapNode := wf.Graph[0].(*ir.Map)

	rig := newMapRig(t,
		execProgram{cmd: "./vote a", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":true}`)}},
		execProgram{cmd: "./vote b", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":false}`)}},
	)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("no-reduce map: got (%q, %v), want (ok, nil)", oc, err)
	}
	// No node.completed at the bare map path (only per-item commits).
	if _, ok := rs.LookupCompleted(testMapPath); ok {
		t.Errorf("a non-reduce map must NOT commit a NodeResult at the map path %q", testMapPath)
	}
	// step.vote.ok lifts the per-item array.
	sc := NewScope(rs, wf, "after_map")
	got, err := sc.Resolve(&template.Ref{Segments: []template.Segment{{Ident: "step"}, {Ident: "vote"}, {Ident: "ok"}}})
	if err != nil {
		t.Fatalf("resolve step.vote.ok: %v", err)
	}
	arr, ok := got.([]any)
	if !ok || len(arr) != 2 || arr[0] != true || arr[1] != false {
		t.Errorf("step.vote.ok = %#v, want []any{true,false} (the per-item array)", got)
	}
}

func TestRunMapRunReduceWiresContainerAndCommitsAtNodePath(t *testing.T) {
	// Task 11 Step 3: an author run: reducer is wired through runMap — the
	// reducer container is Created, the branch artifacts + manifest are staged,
	// the command runs, and its typed output + artifact commit at the MAP path.
	rowSchema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"k"},
		"properties":           map[string]any{"k": map[string]any{"type": "string"}},
	}
	body := ir.NodeList{
		&ir.CodeStep{ID: "scan", Run: "./scan {{ x }}", Container: testMapContainer,
			OutputSchema: rowSchema,
			OutputFiles:  ir.OutputFiles{{Name: "row", Path: "/out/row.csv"}},
			Retry:        &ir.RetryPolicy{Attempts: 1}},
	}
	reduceSchema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"csv_rows"},
		"properties":           map[string]any{"csv_rows": map[string]any{"type": "integer"}},
	}
	wf := staticOverWorkflow("x", body, 1, nil)
	wf.Containers["agg"] = ir.Container{Image: "oci://example.com/r@sha256:" + strings.Repeat("2", 64)}
	mapNode := wf.Graph[0].(*ir.Map)
	mapNode.Reduce = &ir.Reduce{
		Run:          "./merge.sh",
		Container:    "agg",
		OutputSchema: reduceSchema,
		OutputFiles:  ir.OutputFiles{{Name: "csv", Path: "/out/versions.csv"}},
	}

	rig := newMapRig(t)
	rig.fake.ProgramExecWithFiles("./scan a", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"k":"a"}`)}, nil,
		map[string][]byte{"/out/row.csv": []byte("row-a")})
	rig.fake.ProgramExecWithFiles("./scan b", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"k":"b"}`)}, nil,
		map[string][]byte{"/out/row.csv": []byte("row-b")})
	rig.fake.ProgramExecWithFiles("./merge.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"csv_rows":2}`)}, nil,
		map[string][]byte{"/out/versions.csv": []byte("merged")})

	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("run reduce: got (%q, %v), want (ok, nil)", oc, err)
	}

	// The reducer committed its typed output + artifact at the MAP path.
	nr, ok := rs.LookupCompleted(testMapPath)
	if !ok {
		t.Fatalf("no NodeResult committed at the map path %q", testMapPath)
	}
	if nr.Outputs["csv_rows"] != float64(2) {
		t.Errorf("reduced csv_rows = %v, want 2", nr.Outputs["csv_rows"])
	}
	ref, ok := nr.Files["/out/versions.csv"]
	if !ok {
		t.Fatalf("reduced node has no artifact at /out/versions.csv")
	}
	got, gerr := rig.blobs.Get(ref)
	if gerr != nil {
		t.Fatalf("Get reducer artifact: %v", gerr)
	}
	if string(got) != "merged" {
		t.Errorf("reducer artifact = %q, want merged", got)
	}

	// step.scan.files.csv (from outside) resolves to the REDUCER's artifact.
	sc := NewScope(rs, wf, "after_map")
	cas, err := sc.ResolveArtifactPath("scan", "/out/versions.csv")
	if err != nil {
		t.Fatalf("ResolveArtifactPath(scan, /out/versions.csv): %v", err)
	}
	if cas != ref {
		t.Errorf("ResolveArtifactPath = %q, want the reducer's ref %q", cas, ref)
	}
}

// copyToSpy wraps the fake and records every CopyTo call (handle ID → staged
// files), so a test can assert the bytes staged into per-item containers that
// the map executor Destroys before the test can CaptureFiles them. Thread-safe
// (map items dispatch concurrently).
type copyToSpy struct {
	*container.Fake
	mu     sync.Mutex
	staged map[string][]container.InputFile // handle ID → files staged into it
}

func newCopyToSpy(fake *container.Fake) *copyToSpy {
	return &copyToSpy{Fake: fake, staged: map[string][]container.InputFile{}}
}

func (s *copyToSpy) CopyTo(ctx context.Context, h container.Handle, files []container.InputFile) error {
	if err := s.Fake.CopyTo(ctx, h, files); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]container.InputFile, 0, len(files))
	for _, f := range files {
		dup := make([]byte, len(f.Content))
		copy(dup, f.Content)
		cp = append(cp, container.InputFile{Path: f.Path, Content: dup})
	}
	s.staged[h.ID] = append(s.staged[h.ID], cp...)
	return nil
}

func TestRunInputFilesMapBodyConsumesTopLevelProducer(t *testing.T) {
	// SP1 C5 ("one recon doc → N hunters"): a TOP-LEVEL producer `recon` with a
	// named artifact, then a `map` over N items whose body step input_files the
	// recon artifact into the per-item container. Proves ResolveArtifactPath
	// resolves a top-level producer from INSIDE a map body (stepRuntimePath
	// returns the producer's path unchanged) and WithItemHandle routes CopyTo to
	// the per-item container.
	clk := &clock.Fake{T: testClockEpoch}
	fake := container.NewFake()
	spy := newCopyToSpy(fake)
	reconH, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab: %v", err)
	}
	// Base handle for the map's container name (per-item handles are minted on
	// top via WithItemHandle; the map dispatch path still expects an entry).
	boxBase, err := fake.Create(context.Background(), container.ContainerSpec{Name: "box"})
	if err != nil {
		t.Fatalf("Create box: %v", err)
	}
	disp := &LocalDispatcher{
		Backend: spy,
		Handles: map[string]container.Handle{"lab": reconH, "box": boxBase},
	}
	lg := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()

	sentinel := []byte("recon doc\n")
	fake.ProgramExec("./recon.sh", container.ExecResult{ExitCode: 0}, nil)
	if err := fake.WriteFile(reconH, "/out/report.md", sentinel); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fake.ProgramExec("./hunt.sh a", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("./hunt.sh b", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("./hunt.sh c", container.ExecResult{ExitCode: 0}, nil)

	seedRunStartedWithInput(t, lg, blobs, runOverItems("a", "b", "c"))

	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{
			"lab": {Image: "oci://example.com/r@sha256:" + strings.Repeat("0", 64)},
			"box": {Image: "oci://example.com/r@sha256:" + strings.Repeat("1", 64)},
		},
		Input: &ir.JSONSchema{
			"type":       "object",
			"properties": map[string]any{"items": map[string]any{"type": "array"}},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID: "recon", Container: "lab", Run: "./recon.sh",
				OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/report.md"}},
			},
			&ir.Map{
				Over: ir.Expr("{{ input.items }}"), As: "h", Container: "box", Concurrency: 1,
				Body: ir.NodeList{
					&ir.CodeStep{
						ID: "hunt", Container: "box", Run: "./hunt.sh {{ h }}",
						InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"},
					},
				},
			},
		},
	}
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b", "c"))

	oc, err := Run(context.Background(), &ir.LoadedDefinition{Workflow: wf}, rs, disp, lg, blobs, clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	// The map is the SECOND top-level node (index 1) → path "map[1]".
	items := rs.LookupMapItems("map[1]")
	if len(items) != 3 {
		t.Fatalf("MapItems len = %d, want 3", len(items))
	}
	for _, mr := range items {
		if mr.Status != ItemPassed {
			t.Errorf("item N=%d status=%q, want item_passed", mr.N, mr.Status)
		}
	}
	// Three per-item containers each received the recon doc at /work/report.md.
	stagedCount := 0
	spy.mu.Lock()
	defer spy.mu.Unlock()
	for hID, files := range spy.staged {
		for _, f := range files {
			if f.Path == "/work/report.md" {
				stagedCount++
				if string(f.Content) != string(sentinel) {
					t.Errorf("handle %s staged %q, want %q", hID, f.Content, sentinel)
				}
			}
		}
	}
	if stagedCount != 3 {
		t.Errorf("recon doc staged into %d item containers, want 3", stagedCount)
	}
}

func TestMapItemRecordsRetryableOutcome(t *testing.T) {
	rig := newMapRig(t, fail("echo a")) // exit 1 → retryable_failure
	input := runOverItems("a")
	seedRunStartedWithInput(t, rig.lg, rig.blobs, input)
	minSuccess := ir.Ratio("1")
	wf := staticOverWorkflow("x", echoStep("x", &ir.RetryPolicy{Attempts: 1}), 1, &minSuccess)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, input)

	_, _ = runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)

	// Fold the log (resume's path) — the folded record must carry the outcome.
	rs2 := foldFromRig(t, rig)
	items := rs2.LookupMapItems(testMapPath)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].Status != ItemFailed {
		t.Errorf("Status = %q, want %q", items[0].Status, ItemFailed)
	}
	if items[0].Outcome != string(OutcomeRetryableFailure) {
		t.Errorf("Outcome = %q, want %q", items[0].Outcome, OutcomeRetryableFailure)
	}
}

func TestFoldMapItemLastWinsByN(t *testing.T) {
	clk := &clock.Fake{T: testClockEpoch}
	lg := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	seedRunStartedWithInput(t, lg, blobs, runOverItems("a"))
	for _, d := range []MapItemData{
		{N: 0, Status: ItemFailed, Outcome: "retryable_failure"},
		{N: 0, Status: ItemPassed},
	} {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := lg.Append(state.Event{Type: EventMapItem, Path: testMapPath, Data: b}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	events, err := lg.Fold()
	if err != nil {
		t.Fatalf("Fold log: %v", err)
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := rs.LookupMapItems(testMapPath)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (last-wins by N)", len(got))
	}
	if got[0].Status != ItemPassed {
		t.Errorf("Status = %q, want %q (last event wins)", got[0].Status, ItemPassed)
	}
}

// Pure predicate: who re-runs on resume. Keyed on the existing MapItemRecord
// (no parallel projection type — rule 2).
func TestShouldRerunItem(t *testing.T) {
	plain := &ir.Map{}                   // n.Prune == nil
	prune := &ir.Map{Prune: &ir.Prune{}} // any non-nil Prune
	cases := []struct {
		name   string
		n      *ir.Map
		resume bool
		mr     MapItemRecord
		want   bool
	}{
		{"retryable-on-resume", plain, true, MapItemRecord{Status: ItemFailed, Outcome: "retryable_failure"}, true},
		{"permanent-stays", plain, true, MapItemRecord{Status: ItemFailed, Outcome: "permanent_failure"}, false},
		{"rejected-stays", plain, true, MapItemRecord{Status: ItemFailed, Outcome: "rejected"}, false},
		{"absent-outcome-stays", plain, true, MapItemRecord{Status: ItemFailed, Outcome: ""}, false},
		{"image-unavailable-stays", plain, true, MapItemRecord{Status: ItemFailed, Outcome: "", Reason: ReasonImageUnavailable}, false},
		{"passed-stays", plain, true, MapItemRecord{Status: ItemPassed}, false},
		{"pruned-stays", plain, true, MapItemRecord{Status: ItemPruned}, false},
		{"prune-map-never", prune, true, MapItemRecord{Status: ItemFailed, Outcome: "retryable_failure"}, false},
		{"not-resume-never", plain, false, MapItemRecord{Status: ItemFailed, Outcome: "retryable_failure"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRerunItem(c.n, c.resume, c.mr); got != c.want {
				t.Errorf("shouldRerunItem = %v, want %v", got, c.want)
			}
		})
	}
}

// Integration: a retryable item recovers on resume → map ok, single record per N.
func runMapResumeTrue(ctx context.Context, n *ir.Map, mapPath string, wf *ir.Workflow, rs *RunState, rig *mapRig) (Outcome, error) {
	return runMapWithContext(ctx, n, mapPath, interpreterContext{
		wf: wf, runstate: rs, dispatcher: rig.ld, log: rig.lg, blobs: rig.blobs, clk: rig.clk, resume: true,
	})
}

func TestRunMapResumeRetryableItemReRuns(t *testing.T) {
	rig1 := newMapRig(t, ok("echo a"), fail("echo b"), ok("echo c"))
	input := runOverItems("a", "b", "c")
	seedRunStartedWithInput(t, rig1.lg, rig1.blobs, input)
	minSuccess := ir.Ratio("3")
	wf := staticOverWorkflow("x", echoStep("x", &ir.RetryPolicy{Attempts: 1}), 3, &minSuccess)
	mapNode := wf.Graph[0].(*ir.Map)
	rs1 := NewRunState(testRunID, testDigest, input)

	oc1, _ := runMap(context.Background(), mapNode, testMapPath, wf, rs1, rig1.ld, rig1.lg, rig1.blobs, rig1.clk, nil, nil)
	if oc1 != OutcomeRetryableFailure {
		t.Fatalf("round-1 outcome = %q, want OutcomeRetryableFailure", oc1)
	}

	rig2 := bareRig(t, rig1, ok("echo a"), ok("echo b"), ok("echo c"))
	rs2 := foldFromRig(t, rig2)

	oc2, err2 := runMapResumeTrue(context.Background(), mapNode, testMapPath, wf, rs2, rig2)
	if oc2 != OutcomeOK || err2 != nil {
		t.Fatalf("resume outcome = %q err = %v, want OutcomeOK/nil (retryable item recovered)", oc2, err2)
	}
	if len(rig2.fake.Calls) == 0 {
		t.Error("resume did NOT re-run the failed item (fake.Calls empty)")
	}
	// Single record per N after re-run.
	seen := map[int]bool{}
	for _, mr := range rs2.LookupMapItems(testMapPath) {
		if seen[mr.N] {
			t.Errorf("duplicate MapItemRecord for N=%d", mr.N)
		}
		seen[mr.N] = true
	}
}
