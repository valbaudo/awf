package engine

import (
	"encoding/json"
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
		out.OutputsRef != in.OutputsRef || out.Files["/out/a"] != in.Files["/out/a"] {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
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
