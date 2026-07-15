package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

func TestEventTypeConstants(t *testing.T) {
	// Pin the exact string values — these are the wire format. Renaming any of
	// these would invalidate every existing log.
	cases := map[string]string{
		EventRunStarted:     "run.started",
		EventCallStarted:    "call.started",
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
		EventSkillsSelected: "skills.selected",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("event type constant = %q, want %q", got, want)
		}
	}
}

func TestItemPrunedStatusConstant(t *testing.T) {
	// Pin the exact wire string of the third terminal map-item status (SP5).
	// Renaming it would invalidate every committed map.item{item_pruned} event.
	if ItemPruned != "item_pruned" {
		t.Errorf("ItemPruned = %q, want %q", ItemPruned, "item_pruned")
	}
}

func TestTallyResultsIgnoresPruned(t *testing.T) {
	// A pruned item counts as NEITHER a pass NOR a failure: it is removed from
	// both the numerator and (via Task 4) the min_success denominator.
	pass, fail := tallyResults([]string{ItemPassed, ItemPruned, ItemFailed})
	if pass != 1 || fail != 1 {
		t.Errorf("tallyResults(passed,pruned,failed) = (pass=%d, fail=%d), want (1, 1)", pass, fail)
	}
}

func TestRunStartedDataRoundTrip(t *testing.T) {
	in := RunStartedData{
		RunID:          "deadbeef",
		WorkflowDigest: "awf-d1:sha256:abc",
		InputRef:       "awf-d1:sha256:def",
		LiveHome:       &LiveHomePin{Path: "/state/live", Digest: "sha256:live"},
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
		out.InputRef != in.InputRef || out.LiveHome == nil || *out.LiveHome != *in.LiveHome ||
		len(out.Runtimes) != 0 {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestCallStartedDataRoundTrip(t *testing.T) {
	in := CallStartedData{
		InputRef:   "awf-d1:sha256:def",
		InputFiles: map[string]string{"report": "sha256:report"},
		Runtimes: []ResolvedRuntime{
			{Ref: "anthropic/claude-code", Version: "2.1.118", Container: "lab"},
			{Ref: "openai/codex", Version: "0.31.0"},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"input_ref":"awf-d1:sha256:def"`)) {
		t.Errorf("marshal = %s, want input_ref", b)
	}
	if !bytes.Contains(b, []byte(`"input_files":{"report":"sha256:report"}`)) {
		t.Errorf("marshal = %s, want input_files", b)
	}
	if bytes.Contains(b, []byte("workflow_digest")) {
		t.Errorf("call.started JSON must not contain workflow_digest, got %s", b)
	}
	var out CallStartedData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.InputRef != in.InputRef {
		t.Errorf("InputRef = %q, want %q", out.InputRef, in.InputRef)
	}
	if out.InputFiles["report"] != "sha256:report" {
		t.Errorf("InputFiles[report] = %q, want sha256:report", out.InputFiles["report"])
	}
	if len(out.Runtimes) != len(in.Runtimes) {
		t.Fatalf("len(Runtimes) = %d, want %d", len(out.Runtimes), len(in.Runtimes))
	}
	for i := range in.Runtimes {
		if out.Runtimes[i] != in.Runtimes[i] {
			t.Errorf("Runtimes[%d] = %+v, want %+v", i, out.Runtimes[i], in.Runtimes[i])
		}
	}
}

func TestRunStartedDataRoundTripAssets(t *testing.T) {
	in := RunStartedData{
		RunID:          "deadbeef",
		WorkflowDigest: "awf-d1:sha256:abc",
		Assets: map[string]RunStartedAsset{
			"fixtures": {
				DeclaredPath: "fixtures",
				IsDir:        true,
				Files: []RunStartedAssetFile{
					{Path: "a.txt", Ref: "awf-d1:sha256:aaa", Size: 1, SHA256: "aaa"},
				},
			},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"assets"`)) || bytes.Contains(b, []byte(`"bytes"`)) {
		t.Fatalf("run.started assets JSON shape = %s", b)
	}
	var out RunStartedData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := out.Assets["fixtures"]
	if got.DeclaredPath != "fixtures" || !got.IsDir || len(got.Files) != 1 {
		t.Fatalf("decoded asset = %#v", got)
	}
	if got.Files[0].Path != "a.txt" || got.Files[0].Ref != "awf-d1:sha256:aaa" || got.Files[0].Size != 1 || got.Files[0].SHA256 != "aaa" {
		t.Fatalf("decoded asset file = %#v", got.Files[0])
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

func TestResolvedRuntime_ContainerField_Roundtrip(t *testing.T) {
	in := RunStartedData{
		RunID:          "r1",
		WorkflowDigest: "sha256:x",
		Runtimes: []ResolvedRuntime{
			{Ref: "anthropic/claude-code", Version: "2.1.118", Container: "lab"},
			{Ref: "anthropic/claude-code", Version: "2.0.5", Container: "scratch"},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got RunStartedData
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Runtimes) != 2 {
		t.Fatalf("len(Runtimes) = %d, want 2", len(got.Runtimes))
	}
	for i := range in.Runtimes {
		if got.Runtimes[i] != in.Runtimes[i] {
			t.Errorf("Runtimes[%d] = %+v, want %+v", i, got.Runtimes[i], in.Runtimes[i])
		}
	}
}

func TestResolvedRuntime_ContainerField_OmitemptyWhenEmpty(t *testing.T) {
	// A pre-Phase-5-1 log might persist {Ref, Version} without Container.
	// Verify that Container's omitempty means a struct with only Ref+Version
	// marshals to the same JSON shape.
	rr := ResolvedRuntime{Ref: "x", Version: "v1"}
	b, err := json.Marshal(rr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"ref":"x","version":"v1"}`
	if string(b) != want {
		t.Errorf("marshal = %q, want %q (Container omitempty when empty)", string(b), want)
	}
}

// TestResolvedRuntime_LegacyJSON_DecodesContainerEmpty is the
// backward-compatibility lock: a JSON document written by pre-Phase-5-1
// code (no "container" key) MUST decode cleanly with Container == "".
// This is the spec §8 pinning invariant for legacy logs — resume of a
// pre-Phase-5 run under Phase-5 code must not spuriously fail.
func TestResolvedRuntime_LegacyJSON_DecodesContainerEmpty(t *testing.T) {
	var rr ResolvedRuntime
	if err := json.Unmarshal([]byte(`{"ref":"anthropic/claude-code","version":"2.1.118"}`), &rr); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if rr.Ref != "anthropic/claude-code" {
		t.Errorf("Ref = %q, want %q", rr.Ref, "anthropic/claude-code")
	}
	if rr.Version != "2.1.118" {
		t.Errorf("Version = %q, want %q", rr.Version, "2.1.118")
	}
	if rr.Container != "" {
		t.Errorf("Container = %q, want empty (legacy log)", rr.Container)
	}
}

// TestRunStartedData_PrePhase5Log_RuntimesAbsent verifies the broader
// backward-compat: a run.started JSON written before slice 5.1 (no
// "runtimes" key at all) decodes with Runtimes == nil, and re-marshals
// to the same byte-equal shape (no "runtimes":[] inserted).
func TestRunStartedData_PrePhase5Log_RuntimesAbsent(t *testing.T) {
	legacy := `{"run_id":"r1","workflow_digest":"sha256:abc","backend":"docker"}`
	var rs RunStartedData
	if err := json.Unmarshal([]byte(legacy), &rs); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if rs.Runtimes != nil {
		t.Errorf("Runtimes = %v, want nil (legacy log has no runtimes key)", rs.Runtimes)
	}
	// Re-marshal — must byte-equal the input (omitempty on Runtimes).
	got, err := json.Marshal(rs)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(got) != legacy {
		t.Errorf("re-marshal:\n  got:  %s\n  want: %s\n(byte-equal required so Phase-2-4 logs stay readable post-5.1)", got, legacy)
	}
}

func TestAgentEventData_JSONRoundtrip(t *testing.T) {
	in := AgentEventData{
		Kind:          "assistant",
		Stream:        "stdout",
		Size:          1234,
		PayloadInline: []byte(`{"text":"hello"}`),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got AgentEventData
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind != in.Kind || got.Stream != in.Stream || got.Size != in.Size {
		t.Errorf("scalar fields drifted: got %+v want %+v", got, in)
	}
	if string(got.PayloadInline) != string(in.PayloadInline) {
		t.Errorf("PayloadInline: got %q want %q", got.PayloadInline, in.PayloadInline)
	}
}

func TestAgentEventData_OffloadedRefShape(t *testing.T) {
	in := AgentEventData{
		Kind:       "tool_result",
		Stream:     "stdout",
		Size:       12345,
		PayloadRef: "sha256:abc",
	}
	if in.Size <= AgentEventInlineThreshold {
		t.Fatalf("test bug: Size %d must exceed AgentEventInlineThreshold %d to exercise the offloaded-ref branch", in.Size, AgentEventInlineThreshold)
	}
	b, _ := json.Marshal(in)
	if !strings.Contains(string(b), `"payload_ref":"sha256:abc"`) {
		t.Errorf("PayloadRef not serialized: %s", b)
	}
	if strings.Contains(string(b), `"payload_inline"`) {
		t.Errorf("PayloadInline omitempty failed: %s", b)
	}
}

func TestLiveAgentEventPayloadsAreNormalizedAndRedacted(t *testing.T) {
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	blobs := state.NewInMemoryBlobs()
	events := []agent.AgentEvent{{
		Kind:    "assistant_delta",
		Stream:  "stdout",
		Live:    true,
		Payload: []byte("safe sk-liveSECRET123 \x1b]52;c;SECRET\a text\x00"),
		Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: "safe \x1b[31msk-liveSECRET123 text\x1b[0m"},
	}}

	if err := appendAgentEvents(log, blobs, "live", events); err != nil {
		t.Fatalf("appendAgentEvents: %v", err)
	}
	folded, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(folded) != 1 {
		t.Fatalf("folded events = %d, want 1", len(folded))
	}
	var data AgentEventData
	if err := json.Unmarshal(folded[0].Data, &data); err != nil {
		t.Fatalf("unmarshal AgentEventData: %v", err)
	}
	if !data.Live {
		t.Fatalf("Live = false, want true for live adapter events")
	}
	if got := string(data.PayloadInline); got != "safe sk-[redacted]  text" {
		t.Fatalf("PayloadInline = %q, want sanitized live payload", got)
	}
	if data.DisplayClass != "assistant_delta" || data.DisplaySummary != "safe sk-[redacted] text" {
		t.Fatalf("display metadata = class %q summary %q, want assistant_delta/safe sk-[redacted] text", data.DisplayClass, data.DisplaySummary)
	}
}

func TestLiveAgentEventDisplayMetadataIsBounded(t *testing.T) {
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	blobs := state.NewInMemoryBlobs()
	long := "sk-liveSECRET123 " + strings.Repeat("x", agentEventDisplayFieldLimit+200)
	events := []agent.AgentEvent{{
		Kind:    "assistant_delta",
		Stream:  "stdout",
		Live:    true,
		Payload: []byte("ok"),
		Display: agent.EventDisplay{
			Class: agent.DisplayAssistantDelta,
			Tool:  long,
			Text:  long,
		},
	}}

	if err := appendAgentEvents(log, blobs, "live", events); err != nil {
		t.Fatalf("appendAgentEvents: %v", err)
	}
	folded, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var data AgentEventData
	if err := json.Unmarshal(folded[0].Data, &data); err != nil {
		t.Fatalf("unmarshal AgentEventData: %v", err)
	}
	for name, got := range map[string]string{"DisplayTool": data.DisplayTool, "DisplaySummary": data.DisplaySummary} {
		if strings.Contains(got, "liveSECRET123") {
			t.Fatalf("%s leaked secret in %q", name, got)
		}
		if !strings.Contains(got, "sk-[redacted]") {
			t.Fatalf("%s = %q, want redacted secret marker", name, got)
		}
		if !strings.Contains(got, "[truncated") {
			t.Fatalf("%s = %q, want truncation marker", name, got)
		}
		if len(got) > agentEventDisplayFieldLimit+64 {
			t.Fatalf("%s len = %d, want bounded close to %d", name, len(got), agentEventDisplayFieldLimit)
		}
	}
}

func TestLiveCoalescingFlushesDeltaBeforeToolErrorFinal(t *testing.T) {
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	blobs := state.NewInMemoryBlobs()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := newAgentEventSink(ctx, cancel, &clock.Fake{T: time.Now()}, log, blobs, "live")
	send := func(ev agent.AgentEvent) {
		t.Helper()
		if err := sink.send(ev); err != nil {
			t.Fatalf("sink.send(%s): %v", ev.Kind, err)
		}
	}
	send(agent.AgentEvent{Kind: "delta", Stream: "stdout", Live: true, Payload: []byte("hel"), Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: "hel"}})
	send(agent.AgentEvent{Kind: "delta", Stream: "stdout", Live: true, Payload: []byte("lo"), Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: "lo"}})
	send(agent.AgentEvent{Kind: "tool", Stream: "stdout", Live: true, Payload: []byte("tool"), Display: agent.EventDisplay{Class: agent.DisplayToolCall, Tool: "Shell", Text: "ls"}})
	send(agent.AgentEvent{Kind: "delta", Stream: "stdout", Live: true, Payload: []byte("done"), Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: "done"}})
	send(agent.AgentEvent{Kind: "final", Stream: "stdout", Live: true, Payload: []byte("final"), Display: agent.EventDisplay{Class: agent.DisplayFinal, Text: "final"}})
	if err := sink.closeWait(); err != nil {
		t.Fatalf("closeWait: %v", err)
	}
	folded, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var got []AgentEventData
	for _, ev := range folded {
		var d AgentEventData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("unmarshal AgentEventData: %v", err)
		}
		got = append(got, d)
	}
	if len(got) != 4 {
		t.Fatalf("agent.event count = %d, want 4: %+v", len(got), got)
	}
	if string(got[0].PayloadInline) != "hello" || got[0].DisplayClass != "assistant_delta" || got[0].DisplaySummary != "hello" {
		t.Fatalf("first coalesced delta = %+v, want hello assistant_delta", got[0])
	}
	if got[1].DisplayClass != "tool_call" {
		t.Fatalf("second event = %+v, want tool_call after delta flush", got[1])
	}
	if string(got[2].PayloadInline) != "done" || got[2].DisplayClass != "assistant_delta" {
		t.Fatalf("third event = %+v, want later delta", got[2])
	}
	if got[3].DisplayClass != "final" {
		t.Fatalf("fourth event = %+v, want final", got[3])
	}
}

func TestAgentEventSinkAppendFailureCancelsProvider(t *testing.T) {
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	log.FailAppendAfterN(0)
	blobs := state.NewInMemoryBlobs()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	providerCtx, cancelProvider := context.WithCancel(context.Background())
	sink := newAgentEventSink(ctx, cancelProvider, &clock.Fake{T: time.Now()}, log, blobs, "live")
	if err := sink.send(agent.AgentEvent{Kind: "final", Stream: "stdout", Live: true, Payload: []byte("final"), Display: agent.EventDisplay{Class: agent.DisplayFinal, Text: "final"}}); err != nil {
		t.Fatalf("sink.send: %v", err)
	}
	if err := sink.closeWait(); err == nil {
		t.Fatal("closeWait = nil, want append failure")
	}
	select {
	case <-providerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("provider context was not cancelled after append failure")
	}
}

func TestLocalDispatcherAgentEventSinkDirectErrorReturnsNilChunks(t *testing.T) {
	wantErr := errors.New("sink failed")
	sinkCtx, stopSink := context.WithCancel(context.Background())
	defer stopSink()
	sink := newAgentEventSink(sinkCtx, func() {}, &clock.Fake{T: time.Now()}, state.NewInMemoryLog(clock.System{}), state.NewInMemoryBlobs(), "live")
	sink.setErr(wantErr)
	defer func() {
		if err := sink.closeWait(); !errors.Is(err, wantErr) {
			t.Errorf("closeWait err = %v, want %v", err, wantErr)
		}
	}()

	fk := fake.New("live/agent").
		WithCaps(agent.Caps{NativeSchema: true, Containerless: true}).
		Script(0, fake.Result{
			Events: []agent.AgentEvent{{Kind: "delta", Stream: "stdout", Live: true, Payload: []byte("working")}},
			Output: map[string]any{"ok": true},
		})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	dispatcher := &LocalDispatcher{Resolver: &reg}
	intent := NodeIntent{
		Path:           "live",
		Node:           &ir.AgentStep{ID: "live", Uses: "live/agent"},
		ResolvedInputs: ResolvedInputs{Uses: "live/agent"},
		agentEventSink: sink,
	}

	_, chunks, err := dispatcher.Run(context.Background(), intent)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if chunks != nil {
		t.Fatal("chunks is non-nil on direct sink error")
	}
}

func TestAgentEventSinkBackpressureCancelsProvider(t *testing.T) {
	log := newBlockingLog()
	blobs := state.NewInMemoryBlobs()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	providerCtx, cancelProvider := context.WithCancel(context.Background())
	sink := newAgentEventSink(ctx, cancelProvider, &clock.Fake{T: time.Now()}, log, blobs, "live")
	if err := sink.send(agent.AgentEvent{Kind: "final", Stream: "stdout", Live: true, Payload: []byte("first"), Display: agent.EventDisplay{Class: agent.DisplayFinal, Text: "first"}}); err != nil {
		t.Fatalf("first sink.send: %v", err)
	}
	<-log.appendStarted
	for i := 0; i < agentEventSinkBuffer; i++ {
		if err := sink.send(agent.AgentEvent{Kind: "notice", Stream: "stdout", Live: true, Payload: []byte("queued"), Display: agent.EventDisplay{Class: agent.DisplayNotice, Text: "queued"}}); err != nil {
			t.Fatalf("fill sink.send[%d]: %v", i, err)
		}
	}
	err := sink.send(agent.AgentEvent{Kind: "overflow", Stream: "stdout", Live: true, Payload: []byte("overflow"), Display: agent.EventDisplay{Class: agent.DisplayNotice, Text: "overflow"}})
	if !errors.Is(err, ErrAgentEventSinkBackpressure) {
		t.Fatalf("overflow sink.send err = %v, want ErrAgentEventSinkBackpressure", err)
	}
	select {
	case <-providerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("provider context was not cancelled after backpressure")
	}
	close(log.release)
	if err := sink.closeWait(); !errors.Is(err, ErrAgentEventSinkBackpressure) {
		t.Fatalf("closeWait err = %v, want ErrAgentEventSinkBackpressure", err)
	}
}

type blockingLog struct {
	appendStarted chan struct{}
	release       chan struct{}
	once          sync.Once
}

func newBlockingLog() *blockingLog {
	return &blockingLog{appendStarted: make(chan struct{}), release: make(chan struct{})}
}

func (l *blockingLog) Append(state.Event) error {
	l.once.Do(func() { close(l.appendStarted) })
	<-l.release
	return nil
}

func (*blockingLog) Sync() error { return nil }

func (*blockingLog) Fold() ([]state.Event, error) { return nil, nil }

func (*blockingLog) Close() error { return nil }

func TestAgentEventData_FoldIgnores(t *testing.T) {
	// agent.event events are observational, like retry.attempt. Fold must
	// not mutate RunState in response to them. We assert this by Fold'ing a
	// log containing only run.started + one agent.event, and checking
	// RunState.Completed is empty — Fold must not derive any state from the
	// agent.event.
	log := state.NewInMemoryLog(&clock.Fake{T: time.Now()})
	if err := log.Append(state.Event{Type: EventRunStarted, Data: mustJSON(RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	if err := log.Append(state.Event{
		Type: EventAgentEvent,
		Path: "graph[0]",
		Data: mustJSON(AgentEventData{Kind: "assistant", Stream: "stdout", PayloadInline: []byte(`{"text":"hello"}`)}),
	}); err != nil {
		t.Fatalf("append agent.event: %v", err)
	}
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("engine.Fold: %v", err)
	}
	if len(rs.Completed) != 0 {
		t.Errorf("Fold mutated RunState in response to agent.event: Completed=%+v", rs.Completed)
	}
}

func TestRunStartedWorkflowIDVersionRoundTrip(t *testing.T) {
	in := RunStartedData{RunID: "r1", WorkflowDigest: "awf-d1:sha256:abc", WorkflowID: "cve-pipeline", WorkflowVersion: 1}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out RunStartedData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.WorkflowID != "cve-pipeline" || out.WorkflowVersion != 1 {
		t.Errorf("round-trip = %q/%d, want cve-pipeline/1", out.WorkflowID, out.WorkflowVersion)
	}
	// Legacy decode: a pre-6.1 run.started (no workflow_id) yields empty/zero.
	var legacy RunStartedData
	if err := json.Unmarshal([]byte(`{"run_id":"r0","workflow_digest":"d"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.WorkflowID != "" || legacy.WorkflowVersion != 0 {
		t.Errorf("legacy decode = %q/%d, want empty/0", legacy.WorkflowID, legacy.WorkflowVersion)
	}
}

// mustJSON is a test helper — panics on marshal failure (only used with
// types we control whose marshalling can't actually fail).
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestRunFinishedDataFromEvent(t *testing.T) {
	e := state.Event{Type: EventRunFinished, Data: []byte(`{"outcome":"permanent_failure"}`)}
	d, err := RunFinishedDataFromEvent(e)
	if err != nil {
		t.Fatalf("RunFinishedDataFromEvent: %v", err)
	}
	if d.Outcome != "permanent_failure" {
		t.Fatalf("Outcome = %q, want permanent_failure", d.Outcome)
	}
	if _, err := RunFinishedDataFromEvent(state.Event{Type: EventRunFinished, Data: []byte(`{`)}); err == nil {
		t.Fatal("expected error on corrupt run.finished payload")
	}
}

func TestNodeFailedDataFromEvent(t *testing.T) {
	// node.failed outcomes are only retryable_failure / permanent_failure (see
	// NodeFailedData doc); a gate's "rejected" rolls up via run.finished, never
	// a node.failed event.
	e := state.Event{Type: EventNodeFailed, Data: []byte(`{"outcome":"permanent_failure","error":"x"}`)}
	d, err := NodeFailedDataFromEvent(e)
	if err != nil {
		t.Fatalf("NodeFailedDataFromEvent: %v", err)
	}
	if d.Outcome != "permanent_failure" {
		t.Fatalf("Outcome = %q, want permanent_failure", d.Outcome)
	}
	if _, err := NodeFailedDataFromEvent(state.Event{Type: EventNodeFailed, Data: []byte(`{`)}); err == nil {
		t.Fatal("expected error on corrupt node.failed payload")
	}
}

func TestReactRoundDataRoundTrip(t *testing.T) {
	in := ReactRoundData{N: 3}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"n":3}` {
		t.Fatalf("on-wire = %s, want {\"n\":3}", b)
	}
	var out ReactRoundData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.N != 3 {
		t.Fatalf("N = %d, want 3", out.N)
	}
}
