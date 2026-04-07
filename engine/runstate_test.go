package engine

import "testing"

func TestParseOutcome(t *testing.T) {
	cases := []struct {
		in      string
		want    Outcome
		wantErr bool
	}{
		{"ok", OutcomeOK, false},
		{"retryable_failure", OutcomeRetryableFailure, false},
		{"permanent_failure", OutcomePermanentFailure, false},
		{"", "", true},
		{"OK", "", true},               // case-sensitive
		{"success", "", true},          // not a valid AWF outcome (quality is the gate's job)
		{"semantic_failure", "", true}, // ditto
		{"fubar", "", true},
	}
	for _, c := range cases {
		got, err := ParseOutcome(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseOutcome(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
		if got != c.want {
			t.Errorf("ParseOutcome(%q) = %q, want %q", c.in, got, c.want)
		}
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

func TestNodeResultIsCopyable(t *testing.T) {
	// NodeResult is stored by value in RunState.Completed; we rely on Go's map-value
	// semantics. This pins that copying preserves all fields.
	exit := 0
	original := NodeResult{
		Outcome:    OutcomeOK,
		ExitCode:   &exit,
		Outputs:    map[string]any{"k": "v"},
		OutputsRef: "awf-d1:sha256:abc",
		Files:      map[string]string{"/out/a": "awf-d1:sha256:def"},
	}
	cp := original
	if cp.Outcome != OutcomeOK || cp.OutputsRef != "awf-d1:sha256:abc" {
		t.Errorf("NodeResult copy lost fields: %+v", cp)
	}
	if cp.Outputs["k"] != "v" || cp.Files["/out/a"] != "awf-d1:sha256:def" {
		t.Errorf("NodeResult copy lost map entries: %+v", cp)
	}
}
