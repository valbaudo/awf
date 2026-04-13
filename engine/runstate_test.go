package engine

import "testing"

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
