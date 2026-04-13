package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventTypeConstants(t *testing.T) {
	// Pin the exact string values — these are the wire format. Renaming any of
	// these would invalidate every existing log.
	cases := map[string]string{
		EventRunStarted:    "run.started",
		EventRunResumed:    "run.resumed",
		EventNodeCompleted: "node.completed",
		EventBranchTaken:   "branch.taken",
		EventLoopIter:      "loop.iter",
		EventRetryAttempt:  "retry.attempt",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("event type constant = %q, want %q", got, want)
		}
	}
}

func TestRunStartedDataRoundTrip(t *testing.T) {
	in := RunStartedData{
		RunID:          "deadbeef",
		WorkflowDigest: "awf-d1:sha256:abc",
		InputRef:       "awf-d1:sha256:def",
		Runtimes:       []ResolvedRuntime{}, // Phase 2: always empty
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RunStartedData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.RunID != in.RunID || out.WorkflowDigest != in.WorkflowDigest ||
		out.InputRef != in.InputRef || len(out.Runtimes) != 0 {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestNodeCompletedDataRoundTrip(t *testing.T) {
	exit := 0
	in := NodeCompletedData{
		Outcome:    "ok",
		ExitCode:   &exit,
		OutputsRef: "awf-d1:sha256:abc",
		StdoutRef:  "awf-d1:sha256:stdout",
		Files:      map[string]string{"/out/a": "awf-d1:sha256:def"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out NodeCompletedData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Outcome != "ok" || out.ExitCode == nil || *out.ExitCode != 0 ||
		out.OutputsRef != in.OutputsRef || out.StdoutRef != in.StdoutRef ||
		out.Files["/out/a"] != in.Files["/out/a"] {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestNodeCompletedDataStdoutRefOmitEmpty(t *testing.T) {
	// StdoutRef is omitempty — a step that produced no stdout (or had none, e.g.
	// agent/signal) writes NodeCompletedData without the key. Pin the wire shape
	// both ways so a future writer can't drift to `"stdout_ref":""` or
	// `"stdout_ref":null`.
	in := NodeCompletedData{Outcome: "ok", StdoutRef: "awf-d1:sha256:stdout"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal with StdoutRef: %v", err)
	}
	if !strings.Contains(string(b), `"stdout_ref":"awf-d1:sha256:stdout"`) {
		t.Errorf("marshal w/ StdoutRef = %s, want substring %q", b, `"stdout_ref":"awf-d1:sha256:stdout"`)
	}

	empty := NodeCompletedData{Outcome: "ok"}
	b, err = json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal without StdoutRef: %v", err)
	}
	if strings.Contains(string(b), "stdout_ref") {
		t.Errorf("marshal w/o StdoutRef = %s, must NOT contain %q (omitempty)", b, "stdout_ref")
	}
}

func TestNodeCompletedDataOmitEmpty(t *testing.T) {
	// A node with no output_files / no output_schema produces a NodeCompletedData with
	// no OutputsRef and no Files. Marshal must omit them so the JSON is minimal.
	in := NodeCompletedData{Outcome: "ok"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	want := `{"outcome":"ok"}`
	if got != want {
		t.Errorf("marshal omit-empty = %q, want %q", got, want)
	}
}

func TestBranchTakenDataRoundTrip(t *testing.T) {
	for _, which := range []string{"then", "else"} {
		in := BranchTakenData{Which: which}
		b, _ := json.Marshal(in)
		var out BranchTakenData
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("unmarshal %q: %v", which, err)
		}
		if out.Which != which {
			t.Errorf("BranchTakenData.Which = %q, want %q", out.Which, which)
		}
	}
}

func TestLoopIterDataRoundTrip(t *testing.T) {
	in := LoopIterData{N: 3}
	b, _ := json.Marshal(in)
	var out LoopIterData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.N != 3 {
		t.Errorf("LoopIterData.N = %d, want 3", out.N)
	}
}

func TestRetryAttemptDataRoundTrip(t *testing.T) {
	in := RetryAttemptData{
		N:       2,
		Outcome: string(OutcomeRetryableFailure),
		Error:   "transient transport failure",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"n":2`,
		`"outcome":"retryable_failure"`,
		`"error":"transient transport failure"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("marshal = %s, want substring %q", got, want)
		}
	}

	var out RetryAttemptData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.N != in.N || out.Outcome != in.Outcome || out.Error != in.Error {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
	}

	// Error is omitempty — an attempt that classified as retryable but carried
	// no error message (e.g. a permanent exit code with no transport error)
	// must marshal without the "error" key. Pin the wire shape so a future
	// writer can't drift to `"error":""` or `"error":null`.
	empty := RetryAttemptData{N: 1, Outcome: "retryable_failure"}
	b, err = json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal without Error: %v", err)
	}
	if strings.Contains(string(b), "error") {
		t.Errorf("marshal w/o Error = %s, must NOT contain %q (omitempty)", b, "error")
	}
}
