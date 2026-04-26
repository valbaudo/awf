package engine

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEventTypeConstants(t *testing.T) {
	// Pin the exact string values — these are the wire format. Renaming any of
	// these would invalidate every existing log.
	cases := map[string]string{
		EventRunStarted:     "run.started",
		EventRunResumed:     "run.resumed",
		EventNodeCompleted:  "node.completed",
		EventBranchTaken:    "branch.taken",
		EventLoopIter:       "loop.iter",
		EventRetryAttempt:   "retry.attempt",
		EventNodeSkipped:    "node.skipped",
		EventGateAttempt:    "gate.attempt",
		EventMapItem:        "map.item",
		EventSignalReceived: "signal.received",
		EventRunPaused:      "run.paused",
		EventRunCancelled:   "run.cancelled",
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

func TestNodeFailedDataRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   NodeFailedData
	}{
		{"retryable_with_error", NodeFailedData{Outcome: string(OutcomeRetryableFailure), Error: "exit 1 after 3 attempts: 'connection refused'"}},
		{"permanent_with_error", NodeFailedData{Outcome: string(OutcomePermanentFailure), Error: "exit code 78 (declared non-retryable)"}},
		{"empty_error_is_omitted", NodeFailedData{Outcome: string(OutcomePermanentFailure)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			// Verify omitempty on Error.
			if c.in.Error == "" && strings.Contains(string(b), `"error"`) {
				t.Errorf("empty Error should be omitted, got %s", b)
			}
			var got NodeFailedData
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got != c.in {
				t.Errorf("round-trip mismatch: got %+v, want %+v", got, c.in)
			}
		})
	}
}

func TestRunFinishedDataRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []RunFinishedData{
		{Outcome: string(OutcomeOK)},
		{Outcome: string(OutcomeRetryableFailure)},
		{Outcome: string(OutcomePermanentFailure)},
	}
	for _, in := range cases {
		t.Run(in.Outcome, func(t *testing.T) {
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got RunFinishedData
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got != in {
				t.Errorf("round-trip mismatch: got %+v, want %+v", got, in)
			}
		})
	}
}

func TestNodeSkippedDataRoundTrip(t *testing.T) {
	in := NodeSkippedData{
		Path:   "loop[0].body.iter-2",
		Reason: "triage found no source",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out NodeSkippedData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
	// Both-empty case — both fields omitempty, JSON should be "{}".
	in2 := NodeSkippedData{}
	b2, _ := json.Marshal(in2)
	if string(b2) != "{}" {
		t.Errorf("NodeSkippedData{} JSON = %q, want %q", string(b2), "{}")
	}
}

func TestGateAttemptDataRoundTrip(t *testing.T) {
	d := GateAttemptData{
		N:              2,
		AttemptOutcome: AttemptRejected,
		VerdictRef:     "awf-blob-v1:sha256:abc123",
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got GateAttemptData
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != d {
		t.Errorf("round-trip: got %+v, want %+v", got, d)
	}

	// Wire field names — pin them, since renaming any breaks every existing log.
	wantJSON := `{"n":2,"attempt_outcome":"attempt_rejected","verdict_ref":"awf-blob-v1:sha256:abc123"}`
	if string(b) != wantJSON {
		t.Errorf("on-wire: got %s, want %s", b, wantJSON)
	}

	d2 := GateAttemptData{N: 1, AttemptOutcome: AttemptPassed, VerdictRef: ""}
	b2, err := json.Marshal(d2)
	if err != nil {
		t.Fatalf("Marshal empty VerdictRef: %v", err)
	}
	// With `verdict_ref,omitempty`, the field should be absent from the JSON.
	if strings.Contains(string(b2), "verdict_ref") {
		t.Errorf("empty VerdictRef serialization: %s contains verdict_ref; omitempty should drop it", b2)
	}
}

func TestMapItemDataRoundTrip(t *testing.T) {
	d := MapItemData{
		N:      3,
		Status: ItemPassed,
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got MapItemData
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != d {
		t.Errorf("round-trip: got %+v, want %+v", got, d)
	}

	// Wire field names — pin them, since renaming any breaks every existing log.
	wantJSON := `{"n":3,"status":"item_passed"}`
	if string(b) != wantJSON {
		t.Errorf("on-wire: got %s, want %s", b, wantJSON)
	}

	// item_failed wire-format check.
	d2 := MapItemData{N: 1, Status: ItemFailed}
	b2, err := json.Marshal(d2)
	if err != nil {
		t.Fatalf("Marshal item_failed: %v", err)
	}
	if string(b2) != `{"n":1,"status":"item_failed"}` {
		t.Errorf("item_failed on-wire: got %s, want {\"n\":1,\"status\":\"item_failed\"}", b2)
	}
}

func TestEventTypeConstantsAreStable(t *testing.T) {
	// Locks the wire-format strings. Renaming any of these would invalidate every
	// existing log; CI catches the rename via this test before it ships.
	cases := []struct {
		name      string
		got, want string
	}{
		{"NodeFailed", EventNodeFailed, "node.failed"},
		{"RunFinished", EventRunFinished, "run.finished"},
		{"NodeSkipped", EventNodeSkipped, "node.skipped"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
			}
		})
	}
}

func TestAttemptOutcomeConstantsAreStable(t *testing.T) {
	// Pin the exact string values — these are the wire format for
	// GateAttemptData.AttemptOutcome. Renaming either would invalidate every
	// existing gate.attempt event in persisted logs.
	if AttemptPassed != "attempt_passed" {
		t.Errorf("AttemptPassed = %q, want \"attempt_passed\"", AttemptPassed)
	}
	if AttemptRejected != "attempt_rejected" {
		t.Errorf("AttemptRejected = %q, want \"attempt_rejected\"", AttemptRejected)
	}
}

func TestSignalReceivedDataRoundTrip(t *testing.T) {
	d := SignalReceivedData{
		Name:       "human_review",
		Seq:        3,
		PayloadRef: "sha256:abc...",
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got SignalReceivedData
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != d {
		t.Errorf("round-trip: got %+v, want %+v", got, d)
	}
	// Wire field pin — renames break every existing log.
	wantJSON := `{"name":"human_review","seq":3,"payload_ref":"sha256:abc..."}`
	if string(b) != wantJSON {
		t.Errorf("on-wire: got %s, want %s", b, wantJSON)
	}
	// payload_ref omitempty case (signal with empty payload).
	d2 := SignalReceivedData{Name: "tick", Seq: 1}
	b2, _ := json.Marshal(d2)
	if string(b2) != `{"name":"tick","seq":1}` {
		t.Errorf("empty payload_ref on-wire: got %s, want {\"name\":\"tick\",\"seq\":1}", b2)
	}
}

func TestRunPausedDataRoundTrip(t *testing.T) {
	d := RunPausedData{
		NodePath: "step.triage",
		Reason:   "operator inspection",
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got RunPausedData
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != d {
		t.Errorf("round-trip: got %+v, want %+v", got, d)
	}
	wantJSON := `{"node_path":"step.triage","reason":"operator inspection"}`
	if string(b) != wantJSON {
		t.Errorf("on-wire: got %s, want %s", b, wantJSON)
	}
	// Both fields omitempty.
	d2 := RunPausedData{}
	b2, _ := json.Marshal(d2)
	if string(b2) != `{}` {
		t.Errorf("empty on-wire: got %s, want {}", b2)
	}
}

func TestRunCancelledDataRoundTrip(t *testing.T) {
	d := RunCancelledData{
		Reason: "operator cancel",
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got RunCancelledData
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != d {
		t.Errorf("round-trip: got %+v, want %+v", got, d)
	}
	wantJSON := `{"reason":"operator cancel"}`
	if string(b) != wantJSON {
		t.Errorf("on-wire: got %s, want %s", b, wantJSON)
	}
	// Reason omitempty.
	d2 := RunCancelledData{}
	b2, _ := json.Marshal(d2)
	if string(b2) != `{}` {
		t.Errorf("empty on-wire: got %s, want {}", b2)
	}
}

func TestRunStartedDataLegacyLogDecodesBackendAsEmpty(t *testing.T) {
	// A pre-slice-4.5 log's run.started payload has no "backend" key. The
	// decoder must leave RunStartedData.Backend at "" so the consumer
	// (cli/resume.go) can map "" → BackendDocker as the production default.
	legacyJSON := []byte(`{"run_id":"legacy","workflow_digest":"sha256:abc","input_ref":""}`)
	var d RunStartedData
	if err := json.Unmarshal(legacyJSON, &d); err != nil {
		t.Fatalf("Unmarshal legacy: %v", err)
	}
	if d.Backend != "" {
		t.Errorf("Backend = %q, want \"\" (legacy log default)", d.Backend)
	}
	if d.RunID != "legacy" || d.WorkflowDigest != "sha256:abc" {
		t.Errorf("RunID/WorkflowDigest decode lost; got %+v", d)
	}
}

func TestRunStartedDataRoundTripBackendField(t *testing.T) {
	cases := []struct {
		name       string
		backend    string
		wantOnWire string
	}{
		{"empty-omitted", "", `"workflow_digest":"sha256:x"`},
		{"fake", BackendFake, `"backend":"fake"`},
		{"docker", BackendDocker, `"backend":"docker"`},
		{"native", BackendNative, `"backend":"native"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := RunStartedData{RunID: "r1", WorkflowDigest: "sha256:x", Backend: c.backend}
			got, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !bytes.Contains(got, []byte(c.wantOnWire)) {
				t.Errorf("on-wire JSON = %s, want substring %q", got, c.wantOnWire)
			}
			if c.backend == "" && bytes.Contains(got, []byte(`"backend"`)) {
				t.Errorf("on-wire JSON contains backend key for empty value: %s", got)
			}
			var back RunStartedData
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if back.Backend != c.backend {
				t.Errorf("round-trip Backend = %q, want %q", back.Backend, c.backend)
			}
		})
	}
}
