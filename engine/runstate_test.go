package engine

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

func TestParseOutcome(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    Outcome
		wantErr bool
	}{
		{"ok", "ok", OutcomeOK, false},
		{"retryable_failure", "retryable_failure", OutcomeRetryableFailure, false},
		{"permanent_failure", "permanent_failure", OutcomePermanentFailure, false},
		{"empty_string", "", "", true},
		{"wrong_case", "OK", "", true},
		{"success_rejected", "success", "", true}, // not a valid AWF outcome — quality is the gate's job
		{"semantic_failure_rejected", "semantic_failure", "", true},
		{"garbage", "fubar", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseOutcome(c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("ParseOutcome(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("ParseOutcome(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestParseOutcome_ErrorMessage(t *testing.T) {
	_, err := ParseOutcome("fubar")
	if err == nil {
		t.Fatalf("ParseOutcome(\"fubar\") returned nil error")
	}
	// The error message should name the offender + list valid values so a corrupt-log
	// failure produces an actionable message.
	msg := err.Error()
	for _, want := range []string{"fubar", "ok", "retryable_failure", "permanent_failure"} {
		if !contains(msg, want) {
			t.Errorf("ParseOutcome error %q missing substring %q", msg, want)
		}
	}
}

// contains is a tiny test helper — strings.Contains without the import (matches the
// pattern in ir/diagnostic_test.go where the same helper is defined locally).
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestRunStateZeroValueIsUsable(t *testing.T) {
	var rs RunState
	// Zero value has nil maps — fold (Task 5) initializes them. The interpreter and
	// template evaluator must therefore treat nil and empty identically; we pin that
	// here by asserting reads from a zero value don't panic.
	if _, ok := rs.Completed["nope"]; ok {
		t.Errorf("zero-value RunState.Completed had key")
	}
	if _, ok := rs.Branches["nope"]; ok {
		t.Errorf("zero-value RunState.Branches had key")
	}
	if rs.LoopIters["nope"] != 0 {
		t.Errorf("zero-value RunState.LoopIters non-zero")
	}
	if rs.GateAttempts["nope"] != nil {
		t.Errorf("zero-value RunState.GateAttempts[\"nope\"] = non-nil, want nil")
	}
	if _, ok := rs.LookupCallStarted("nope"); ok {
		t.Errorf("zero-value RunState LookupCallStarted ok=true, want false")
	}
}

func TestNewRunStateAllocatesMaps(t *testing.T) {
	t.Parallel()
	rs := NewRunState("r1", "d1", map[string]any{"k": "v"})
	if rs.RunID != "r1" || rs.WorkflowDigest != "d1" {
		t.Errorf("identity fields wrong: %+v", rs)
	}
	if rs.Epoch != 1 {
		t.Errorf("Epoch = %d, want 1 (first-run baseline)", rs.Epoch)
	}
	if rs.Input["k"] != "v" {
		t.Errorf("Input not preserved")
	}
	rs.Completed["x"] = NodeResult{}
	rs.Branches["y"] = "then"
	rs.LoopIters["z"] = 1
	rs.GateAttempts["z"] = nil // must not panic — map is allocated
	rs.CallStarted["call.z"] = CallStartedRecord{}
}

func TestNewRunStateNilInputIsValid(t *testing.T) {
	t.Parallel()
	rs := NewRunState("r1", "d1", nil)
	if rs.Input != nil {
		t.Errorf("Input = %v, want nil", rs.Input)
	}
	// Maps still allocated even with nil input — assigning must not panic.
	rs.Completed["x"] = NodeResult{}
	rs.Branches["y"] = "then"
	rs.LoopIters["z"] = 1
}

func TestNodeResultCopyIsShallow(t *testing.T) {
	// NodeResult is stored by value in RunState.Completed, but it embeds maps
	// and slices (Outputs, Files, Stdout) which are reference types — copying
	// the struct shares the underlying storage. Downstream code (Phase 2.4/2.5
	// fold callers, template evaluator) must treat RunState.Completed entries
	// as read-only: mutating .Outputs / .Files / .Stdout through a copied
	// NodeResult corrupts the fold-committed record. This test pins that
	// aliasing semantics so a future reader doesn't assume a deep copy.
	exit := 0
	original := NodeResult{
		Outcome:    OutcomeOK,
		ExitCode:   &exit,
		Outputs:    map[string]any{"k": "v"},
		OutputsRef: "awf-d1:sha256:abc",
		Stdout:     []byte("hello"),
		StdoutRef:  "awf-d1:sha256:stdout",
		Files:      map[string]string{"/out/a": "awf-d1:sha256:def"},
	}
	cp := original

	// Scalar / pointer fields are preserved.
	if cp.Outcome != OutcomeOK || cp.OutputsRef != "awf-d1:sha256:abc" ||
		cp.StdoutRef != "awf-d1:sha256:stdout" || cp.ExitCode != &exit {
		t.Errorf("scalar fields not preserved: %+v", cp)
	}

	// Maps are SHARED — mutating cp.Outputs visibly mutates original.Outputs.
	// (If a future refactor makes the copy deep, this test fails and the new
	// invariant must be re-pinned.)
	cp.Outputs["mutated"] = "yes"
	if original.Outputs["mutated"] != "yes" {
		t.Errorf("Outputs map is unexpectedly NOT shared: original=%+v cp=%+v",
			original.Outputs, cp.Outputs)
	}
	cp.Files["/out/b"] = "awf-d1:sha256:newref"
	if original.Files["/out/b"] != "awf-d1:sha256:newref" {
		t.Errorf("Files map is unexpectedly NOT shared: original=%+v cp=%+v",
			original.Files, cp.Files)
	}

	// Slice backing array is SHARED — mutating an element of cp.Stdout visibly
	// mutates original.Stdout (slice 2.4: same READ-ONLY discipline as Outputs
	// and Files).
	cp.Stdout[0] = 'H'
	if original.Stdout[0] != 'H' {
		t.Errorf("Stdout slice is unexpectedly NOT shared: original=%q cp=%q",
			original.Stdout, cp.Stdout)
	}
}

func TestRunStateMethodsRoundTrip(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)

	rs.RecordCompleted("step1", NodeResult{Outcome: OutcomeOK})
	got, ok := rs.LookupCompleted("step1")
	if !ok || got.Outcome != OutcomeOK {
		t.Errorf("Completed round-trip: got (%+v, %v), want (Outcome=ok, true)", got, ok)
	}
	if _, ok := rs.LookupCompleted("nope"); ok {
		t.Errorf("Completed lookup for missing path: ok=true, want false")
	}

	rs.RecordBranch("if[0]", "then")
	if which, ok := rs.LookupBranch("if[0]"); !ok || which != "then" {
		t.Errorf("Branches round-trip: got (%q, %v), want (then, true)", which, ok)
	}

	rs.RecordLoopIter("loop[0]", 3)
	if got := rs.LookupLoopIters("loop[0]"); got != 3 {
		t.Errorf("LoopIters round-trip: got %d, want 3", got)
	}
	if got := rs.LookupLoopIters("loop[nope]"); got != 0 {
		t.Errorf("LoopIters lookup for missing path: got %d, want 0 (zero value)", got)
	}
}

func TestRunStateCallStartedPathsSortedCopy(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordCallStarted("z.call", CallStartedRecord{})
	rs.RecordCallStarted("a.call", CallStartedRecord{})

	got := rs.CallStartedPaths()
	want := []string{"a.call", "z.call"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CallStartedPaths = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if again := rs.CallStartedPaths(); !reflect.DeepEqual(again, want) {
		t.Fatalf("CallStartedPaths returned aliased slice: after mutation got %v, want %v", again, want)
	}
}

func TestRunStateConcurrentAccessAllMaps(t *testing.T) {
	// Stresses Completed + Branches + LoopIters concurrently. Run with
	// `go test -race ./engine/` to verify race-freedom — Phase 3 slice 3.2
	// (parallel) branches mutate all three maps from different goroutines.
	rs := NewRunState("run-x", "digest", nil)
	const N = 32
	var wg sync.WaitGroup
	wg.Add(3 * N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			path := fmt.Sprintf("c-%d.s1", i)
			rs.RecordCompleted(path, NodeResult{Outcome: OutcomeOK})
			if _, ok := rs.LookupCompleted(path); !ok {
				t.Errorf("Completed lookup after write %q: ok=false", path)
			}
		}()
		go func() {
			defer wg.Done()
			path := fmt.Sprintf("if-%d", i)
			rs.RecordBranch(path, "then")
			if which, ok := rs.LookupBranch(path); !ok || which != "then" {
				t.Errorf("Branches lookup after write %q: got (%q, %v)", path, which, ok)
			}
		}()
		go func() {
			defer wg.Done()
			path := fmt.Sprintf("loop-%d", i)
			rs.RecordLoopIter(path, i+1)
			if got := rs.LookupLoopIters(path); got != i+1 {
				t.Errorf("LoopIters lookup after write %q: got %d, want %d", path, got, i+1)
			}
		}()
	}
	wg.Wait()
}

func TestRunStateGateAttemptsRoundTrip(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)

	// Empty state: LookupGateAttempts returns nil (the zero-value slice).
	if got := rs.LookupGateAttempts("gate[0]"); got != nil {
		t.Errorf("empty LookupGateAttempts: got %v, want nil", got)
	}

	// Record one attempt and read back.
	ar1 := AttemptResult{
		N:              1,
		AttemptOutcome: AttemptRejected,
		Verdict:        map[string]any{"verified": false, "feedback": "missing X"},
	}
	rs.RecordGateAttempt("gate[0]", ar1)
	got := rs.LookupGateAttempts("gate[0]")
	if len(got) != 1 {
		t.Fatalf("after 1 record: len = %d, want 1", len(got))
	}
	if got[0].N != 1 || got[0].AttemptOutcome != AttemptRejected {
		t.Errorf("got[0] = %+v, want {N:1, AttemptOutcome:%q}", got[0], AttemptRejected)
	}
	if got[0].Verdict["feedback"] != "missing X" {
		t.Errorf("got[0].Verdict = %+v, want feedback=\"missing X\"", got[0].Verdict)
	}

	// Record a second attempt; order preserved.
	ar2 := AttemptResult{N: 2, AttemptOutcome: AttemptPassed, Verdict: map[string]any{"verified": true}}
	rs.RecordGateAttempt("gate[0]", ar2)
	got = rs.LookupGateAttempts("gate[0]")
	if len(got) != 2 {
		t.Fatalf("after 2 records: len = %d, want 2", len(got))
	}
	if got[0].N != 1 || got[1].N != 2 {
		t.Errorf("order broken: got Ns %d,%d; want 1,2", got[0].N, got[1].N)
	}

	// Disjoint gate path is independent.
	rs.RecordGateAttempt("gate[1]", AttemptResult{N: 1, AttemptOutcome: AttemptPassed})
	if len(rs.LookupGateAttempts("gate[0]")) != 2 {
		t.Errorf("disjoint write affected gate[0]")
	}
	if len(rs.LookupGateAttempts("gate[1]")) != 1 {
		t.Errorf("gate[1] not recorded")
	}
}

func TestRunStateConcurrentGateAttempts(t *testing.T) {
	// Phase 3 slice 3.2 (parallel) introduced concurrent RunState mutation.
	// Slice 3.3's GateAttempts must follow the same thread-safety contract —
	// even though gates themselves run sequentially within a single workflow,
	// a parallel containing two gates (one per branch) WOULD have two
	// goroutines calling RecordGateAttempt on disjoint paths concurrently.
	// Run under -race.
	rs := NewRunState("run-x", "digest", nil)
	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			path := fmt.Sprintf("gate-%d", i)
			rs.RecordGateAttempt(path, AttemptResult{N: 1, AttemptOutcome: AttemptPassed})
			if got := rs.LookupGateAttempts(path); len(got) != 1 || got[0].N != 1 {
				t.Errorf("concurrent path %q: got %v", path, got)
			}
		}()
	}
	wg.Wait()
}

func TestGateAttemptsReturnedSliceIsReadOnly(t *testing.T) {
	// Pin that the returned slice aliases the internal backing array. Callers
	// MUST NOT mutate (per LookupGateAttempts doc-comment) — this test
	// documents that the aliasing EXISTS, so a future defensive-copy refactor
	// breaks the test and forces re-pinning. Same philosophy as
	// TestNodeResultCopyIsShallow.
	rs := NewRunState("r", "d", nil)
	rs.RecordGateAttempt("g", AttemptResult{
		N: 1, AttemptOutcome: AttemptPassed,
		Verdict: map[string]any{"k": "v"},
	})
	got := rs.LookupGateAttempts("g")
	got[0].N = 999
	internal := rs.LookupGateAttempts("g")
	if internal[0].N != 999 {
		t.Errorf("aliasing contract broken: returned slice no longer aliases internal; got N=%d, want 999 (the test asserts aliasing EXISTS — see doc-comment)", internal[0].N)
	}
}

func TestRunStateMapItemsRoundTrip(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)

	// Empty state: LookupMapItems returns nil.
	if got := rs.LookupMapItems("map[0]"); got != nil {
		t.Errorf("empty LookupMapItems: got %v, want nil", got)
	}

	// Record one item and read back.
	mr1 := MapItemRecord{
		N:         0,
		ItemValue: "first-cve",
		Status:    ItemPassed,
	}
	rs.RecordMapItem("map[0]", mr1)
	got := rs.LookupMapItems("map[0]")
	if len(got) != 1 {
		t.Fatalf("after 1 record: len = %d, want 1", len(got))
	}
	if got[0].N != 0 || got[0].Status != ItemPassed {
		t.Errorf("got[0] = %+v, want {N:0, Status:%q}", got[0], ItemPassed)
	}
	if got[0].ItemValue != "first-cve" {
		t.Errorf("got[0].ItemValue = %v, want \"first-cve\"", got[0].ItemValue)
	}

	// Record a second item; order preserved.
	mr2 := MapItemRecord{N: 1, ItemValue: "second-cve", Status: ItemFailed}
	rs.RecordMapItem("map[0]", mr2)
	got = rs.LookupMapItems("map[0]")
	if len(got) != 2 {
		t.Fatalf("after 2 records: len = %d, want 2", len(got))
	}
	if got[0].N != 0 || got[1].N != 1 {
		t.Errorf("order broken: got Ns %d,%d; want 0,1", got[0].N, got[1].N)
	}

	// Disjoint map path is independent.
	rs.RecordMapItem("map[1]", MapItemRecord{N: 0, ItemValue: "other", Status: ItemPassed})
	if len(rs.LookupMapItems("map[0]")) != 2 {
		t.Errorf("disjoint write affected map[0]")
	}
}

func TestRunStateUpdateMapItemValue(t *testing.T) {
	// Post-resume contract (Design Q3): Fold rebuilds MapItems with
	// ItemValue: nil; the map handler calls UpdateMapItemValue to fill
	// in the re-derived over[N] value BEFORE body re-execution.
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordMapItem("map[0]", MapItemRecord{N: 0, ItemValue: nil, Status: ItemPassed})

	rs.UpdateMapItemValue("map[0]", 0, "post-resume-value")

	got := rs.LookupMapItems("map[0]")
	if len(got) != 1 || got[0].ItemValue != "post-resume-value" {
		t.Errorf("UpdateMapItemValue: got %+v, want ItemValue=\"post-resume-value\"", got)
	}
	// Status untouched.
	if got[0].Status != ItemPassed {
		t.Errorf("UpdateMapItemValue clobbered Status: got %q, want %q", got[0].Status, ItemPassed)
	}
}

func TestRunStateUpdateMapItemValueNoMatchIsNoop(t *testing.T) {
	// Defense-in-depth: if the (path, N) pair doesn't exist, UpdateMapItemValue
	// silently no-ops (it's an in-memory mirror update, not a writer of truth).
	rs := NewRunState("run-x", "digest", nil)
	// Should not panic; should not create the entry.
	rs.UpdateMapItemValue("map[0]", 0, "ignored")
	if got := rs.LookupMapItems("map[0]"); got != nil {
		t.Errorf("UpdateMapItemValue on missing entry created one: %+v", got)
	}
}

func TestRunStateConcurrentMapItems(t *testing.T) {
	// Like Task 2 of slice 3.3 for GateAttempts: map handler dispatches
	// goroutines that concurrently RecordMapItem. Run under -race.
	rs := NewRunState("run-x", "digest", nil)
	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			path := fmt.Sprintf("map-%d", i)
			rs.RecordMapItem(path, MapItemRecord{N: 0, ItemValue: i, Status: ItemPassed})
			if got := rs.LookupMapItems(path); len(got) != 1 || got[0].N != 0 {
				t.Errorf("concurrent path %q: got %v", path, got)
			}
		}()
	}
	wg.Wait()
}

func TestMapItemRecordCopyIsShallow(t *testing.T) {
	// Aliasing pin (slice 3.4 design Q3 + C1). LookupMapItems returns a SHALLOW
	// COPY: the slice header is fresh (race-clean for concurrent readers vs
	// updateMapItemStatus), but MapItemRecord.ItemValue (typed `any` pointing
	// at a map) is ALIASED — mutating the underlying map through one path
	// is visible through the other.
	rs := NewRunState("run-x", "digest", nil)
	original := map[string]any{"id": "cve-1"}
	rs.RecordMapItem("map[0]", MapItemRecord{N: 0, ItemValue: original, Status: ItemPassed})

	got := rs.LookupMapItems("map[0]")
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	// ItemValue map IS aliased (mutating the bound map shows through).
	original["id"] = "MUTATED"
	got2 := rs.LookupMapItems("map[0]")
	gotMap2, ok := got2[0].ItemValue.(map[string]any)
	if !ok {
		t.Fatalf("ItemValue type = %T, want map[string]any", got2[0].ItemValue)
	}
	if gotMap2["id"] != "MUTATED" {
		t.Errorf("LookupMapItems deep-copied ItemValue (want aliased per slice 3.4 contract); got id=%v", gotMap2["id"])
	}
}

func TestLookupMapItemsReturnsSliceCopy(t *testing.T) {
	// Slice-header copy pin (slice 3.4 design Q3 + C1). LookupMapItems must
	// NOT return the live backing array — concurrent readers + writers would
	// race on the slice elements. The returned slice header is fresh; mutating
	// it does NOT affect subsequent lookups.
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordMapItem("map[0]", MapItemRecord{N: 0, ItemValue: "a", Status: ItemPassed})

	first := rs.LookupMapItems("map[0]")
	// Mutate the returned slice element's Status.
	first[0].Status = "MUTATED"
	// Re-lookup; should NOT see the mutation (the live backing array was unchanged).
	second := rs.LookupMapItems("map[0]")
	if second[0].Status != ItemPassed {
		t.Errorf("LookupMapItems returned live backing array (mutation persisted); got Status=%q, want %q",
			second[0].Status, ItemPassed)
	}
}

func TestLookupSignalsReturnsCopy(t *testing.T) {
	// Slice-header copy pin (slice 3.5 analogous to TestLookupMapItemsReturnsSliceCopy).
	// LookupSignals must NOT return the live backing slice — concurrent readers +
	// writers would race on the slice elements. The returned slice header is fresh;
	// mutating it does NOT affect subsequent lookups.
	rs := NewRunState("run-x", "digest", nil)
	rs.AppendSignal("human_review", SignalEntry{Seq: 1, PayloadRef: "sha256:abc"})

	first := rs.LookupSignals("human_review")
	// Mutate the returned slice element's PayloadRef.
	first[0].PayloadRef = "MUTATED"
	// Re-lookup; should NOT see the mutation (the live backing array was unchanged).
	second := rs.LookupSignals("human_review")
	if second[0].PayloadRef != "sha256:abc" {
		t.Errorf("LookupSignals returned live backing array (mutation persisted); got PayloadRef=%q, want %q",
			second[0].PayloadRef, "sha256:abc")
	}
}

func TestRunStateSignalsRoundTrip(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)
	if got := rs.LookupSignals("human_review"); got != nil {
		t.Errorf("empty LookupSignals: got %v, want nil", got)
	}

	rs.AppendSignal("human_review", SignalEntry{
		Seq:        1,
		PayloadRef: "sha256:abc",
	})
	got := rs.LookupSignals("human_review")
	if len(got) != 1 {
		t.Fatalf("after 1 append: len = %d, want 1", len(got))
	}
	if got[0].Seq != 1 || got[0].PayloadRef != "sha256:abc" {
		t.Errorf("got[0] = %+v, want {Seq:1, PayloadRef:sha256:abc}", got[0])
	}
	rs.AppendSignal("tick", SignalEntry{Seq: 1})
	if len(rs.LookupSignals("human_review")) != 1 {
		t.Errorf("cross-name write affected human_review")
	}
}

func TestRunStateSignalReceivedAtRoundTrip(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)
	if _, ok := rs.LookupSignalReceivedAt("step.approve"); ok {
		t.Errorf("empty LookupSignalReceivedAt: ok=true, want false")
	}
	entry := SignalReceivedEntry{
		Seq:        1,
		PayloadRef: "sha256:abc",
	}
	rs.RecordSignalReceivedAt("step.approve", entry)
	got, ok := rs.LookupSignalReceivedAt("step.approve")
	if !ok {
		t.Fatal("after record: ok=false, want true")
	}
	if got.Seq != 1 || got.PayloadRef != "sha256:abc" {
		t.Errorf("got %+v", got)
	}
	if _, ok := rs.LookupSignalReceivedAt("step.other"); ok {
		t.Errorf("LookupSignalReceivedAt(other): ok=true, want false")
	}
}

func TestRunStateCallStartedRoundTrip(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)
	if _, ok := rs.LookupCallStarted("call.review"); ok {
		t.Errorf("empty LookupCallStarted: ok=true, want false")
	}
	rec := CallStartedRecord{
		Input:    map[string]any{"task": "audit"},
		InputRef: "awf-d1:sha256:abc",
		Runtimes: []ResolvedRuntime{
			{Ref: "anthropic/claude-code", Version: "2.1.118", Container: "lab"},
		},
	}
	rs.RecordCallStarted("call.review", rec)
	got, ok := rs.LookupCallStarted("call.review")
	if !ok {
		t.Fatal("after record: ok=false, want true")
	}
	if got.InputRef != rec.InputRef || got.Input["task"] != "audit" {
		t.Errorf("got %+v, want InputRef=%q task=audit", got, rec.InputRef)
	}
	if len(got.Runtimes) != 1 || got.Runtimes[0] != rec.Runtimes[0] {
		t.Errorf("Runtimes = %+v, want %+v", got.Runtimes, rec.Runtimes)
	}
	if _, ok := rs.LookupCallStarted("call.other"); ok {
		t.Errorf("LookupCallStarted(other): ok=true, want false")
	}
}

func TestRecordCallStartedCopiesInputFiles(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)
	inputFiles := map[string]string{"report": "sha256:report"}

	rs.RecordCallStarted("call.review", CallStartedRecord{InputFiles: inputFiles})
	inputFiles["report"] = "sha256:mutated"

	got, ok := rs.LookupCallStarted("call.review")
	if !ok {
		t.Fatal("LookupCallStarted(call.review): ok=false")
	}
	if got.InputFiles["report"] != "sha256:report" {
		t.Errorf("InputFiles[report] = %q, want sha256:report", got.InputFiles["report"])
	}
}

func TestRunStatePausedRoundTrip(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)
	if rs.LookupPaused() != nil {
		t.Errorf("initial Paused = %+v, want nil", rs.LookupPaused())
	}
	rs.SetPaused(&PauseMarker{NodePath: "step.x", Reason: "test"})
	got := rs.LookupPaused()
	if got == nil || got.NodePath != "step.x" || got.Reason != "test" {
		t.Errorf("SetPaused/LookupPaused: got %+v", got)
	}
	rs.SetPaused(nil)
	if rs.LookupPaused() != nil {
		t.Errorf("after SetPaused(nil) LookupPaused = %+v, want nil", rs.LookupPaused())
	}
}

func TestRunStateCancelledFlag(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)
	if rs.IsCancelled() {
		t.Errorf("initial Cancelled = true, want false")
	}
	if r := rs.LookupCancelReason(); r != "" {
		t.Errorf("initial CancelReason = %q, want \"\"", r)
	}
	rs.SetCancelled(true)
	rs.SetCancelReason("operator stop")
	if !rs.IsCancelled() {
		t.Errorf("after SetCancelled(true) IsCancelled = false")
	}
	if r := rs.LookupCancelReason(); r != "operator stop" {
		t.Errorf("CancelReason = %q, want \"operator stop\"", r)
	}
}

func TestRunStateConcurrentSignals(t *testing.T) {
	rs := NewRunState("run-x", "digest", nil)
	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			rs.AppendSignal(fmt.Sprintf("name-%d", i), SignalEntry{Seq: 1, PayloadRef: "sha256:test"})
			if got := rs.LookupSignals(fmt.Sprintf("name-%d", i)); len(got) != 1 {
				t.Errorf("concurrent path: got %v", got)
			}
		}()
	}
	wg.Wait()
}

func TestNodeResultTranscriptByValue(t *testing.T) {
	// ThreadTurn is a scalar-pair struct (two strings) — copying NodeResult copies
	// Transcript by value, giving independent storage. This is DISTINCT from the
	// shared-map fields (Outputs/Files) and shared-slice field (Stdout) pinned by
	// TestNodeResultCopyIsShallow. Mutations through the copy do NOT affect the
	// original — document that contract here so future readers know it's intentional.
	original := NodeResult{
		Outcome:    OutcomeOK,
		Transcript: agent.ThreadTurn{User: "u1", Assistant: "a1"},
	}
	cp := original
	// ThreadTurn is two strings — copy is fully independent (unlike Outputs/Files maps).
	cp.Transcript.User = "mutated"
	if original.Transcript.User != "u1" {
		t.Errorf("Transcript.User unexpectedly shared: original=%q cp=%q",
			original.Transcript.User, cp.Transcript.User)
	}
	if original.Transcript.Assistant != "a1" {
		t.Errorf("Transcript.Assistant = %q, want a1", original.Transcript.Assistant)
	}
}

func TestRunStateThreadIndexesMemoizedOncePerRun(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "a", Uses: "u"},
			&ir.AgentStep{ID: "b", Uses: "u", Continues: "a"},
			&ir.AgentStep{ID: "c", Uses: "u", Continues: "b"},
		},
	}
	rs := NewRunState("r", "d", nil)

	// stepPathIndex: must match StepPathIndex(wf) and be stable across calls.
	// Top-level steps are addressed by ID directly (no parent prefix).
	idx1 := rs.stepPathIndex(wf)
	if idx1["b"] != "b" {
		t.Errorf("stepPathIndex[b] = %q, want %q", idx1["b"], "b")
	}
	if !reflect.DeepEqual(idx1, StepPathIndex(wf)) {
		t.Errorf("stepPathIndex does not match StepPathIndex: got %v", idx1)
	}
	idx2 := rs.stepPathIndex(wf)
	if reflect.ValueOf(idx1).Pointer() != reflect.ValueOf(idx2).Pointer() {
		t.Errorf("stepPathIndex: two calls returned different map instances (not memoized)")
	}

	// agentStepByID: must return the same *AgentStep pointers as wf.Graph.
	byID := rs.agentStepByID(wf)
	if byID["b"] == nil || byID["b"].Continues != "a" {
		t.Errorf("agentStepByID[b].Continues = %q, want \"a\"", byID["b"].Continues)
	}
	if byID["a"] != wf.Graph[0].(*ir.AgentStep) {
		t.Errorf("agentStepByID[a] is not the same pointer as wf.Graph[0]")
	}
	// Memoization: second call returns same map instance.
	byID2 := rs.agentStepByID(wf)
	if reflect.ValueOf(byID).Pointer() != reflect.ValueOf(byID2).Pointer() {
		t.Errorf("agentStepByID: two calls returned different map instances (not memoized)")
	}

	// threadTargets: a is continued-from by b; b is continued-from by c; c is a leaf.
	want := map[string]bool{"a": true, "b": true}
	tt := rs.threadTargets(wf)
	if !reflect.DeepEqual(tt, want) {
		t.Errorf("threadTargets = %v, want %v", tt, want)
	}
	// Memoization: second call returns same map instance.
	tt2 := rs.threadTargets(wf)
	if reflect.ValueOf(tt).Pointer() != reflect.ValueOf(tt2).Pointer() {
		t.Errorf("threadTargets: two calls returned different map instances (not memoized)")
	}
}

func TestRunStateThreadIndexesConcurrentAccess(t *testing.T) {
	// Confirm the sync.Once-guarded indexes are race-free: N goroutines call all
	// three accessors concurrently. Run under `go test -race ./engine/`.
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "x", Uses: "u"},
			&ir.AgentStep{ID: "y", Uses: "u", Continues: "x"},
		},
	}
	rs := NewRunState("r", "d", nil)
	const N = 32
	var wg sync.WaitGroup
	wg.Add(3 * N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			idx := rs.stepPathIndex(wf)
			if idx["x"] != "x" {
				t.Errorf("concurrent stepPathIndex[x] = %q, want %q", idx["x"], "x")
			}
		}()
		go func() {
			defer wg.Done()
			byID := rs.agentStepByID(wf)
			if byID["y"] == nil || byID["y"].Continues != "x" {
				t.Errorf("concurrent agentStepByID[y].Continues = %q, want \"x\"", byID["y"].Continues)
			}
		}()
		go func() {
			defer wg.Done()
			tt := rs.threadTargets(wf)
			if !tt["x"] {
				t.Errorf("concurrent threadTargets[x] = false, want true")
			}
		}()
	}
	wg.Wait()
}
