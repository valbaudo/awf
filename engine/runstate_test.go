package engine

import (
	"fmt"
	"sync"
	"testing"
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
