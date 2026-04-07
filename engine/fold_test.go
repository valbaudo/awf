package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/state"
)

// fixedTS is a deterministic timestamp used in test events. The fold doesn't read TS
// (it only carries metadata), but events need a non-zero TS to round-trip cleanly.
var fixedTS = time.Unix(1700000000, 0).UTC()

// mustMarshal panics on marshal error — test helper for cleaner table cases.
func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return b
}

func TestFold_EmptyLog(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	rs, err := Fold(nil, blobs)
	if err != nil {
		t.Fatalf("Fold(empty): %v", err)
	}
	if rs.RunID != "" || rs.Epoch != 0 || len(rs.Completed) != 0 {
		t.Errorf("empty fold should produce zero-value RunState, got %+v", rs)
	}
}

func TestFold_RunStartedSeeds(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	inputRef, err := blobs.Put([]byte(`{"cve_id":"CVE-2024-0001"}`))
	if err != nil {
		t.Fatalf("seed input: %v", err)
	}
	events := []state.Event{
		{
			Seq: 1, Epoch: 0, TS: fixedTS,
			Type: EventRunStarted,
			Data: mustMarshal(t, RunStartedData{
				RunID:          "deadbeef",
				WorkflowDigest: "awf-d1:sha256:wf",
				InputRef:       inputRef,
				Runtimes:       []ResolvedRuntime{},
			}),
		},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if rs.RunID != "deadbeef" {
		t.Errorf("RunID = %q, want deadbeef", rs.RunID)
	}
	if rs.WorkflowDigest != "awf-d1:sha256:wf" {
		t.Errorf("WorkflowDigest = %q", rs.WorkflowDigest)
	}
	if rs.Epoch != 1 {
		t.Errorf("Epoch = %d, want 1", rs.Epoch)
	}
	if rs.Input["cve_id"] != "CVE-2024-0001" {
		t.Errorf("Input = %+v, want {cve_id: CVE-2024-0001}", rs.Input)
	}
}

func TestFold_RunStartedWithoutInput(t *testing.T) {
	// A workflow without an `input:` declaration emits run.started with InputRef="".
	// The fold leaves RunState.Input nil (NOT empty-map).
	events := []state.Event{
		{
			Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: mustMarshal(t, RunStartedData{
				RunID:          "abc",
				WorkflowDigest: "awf-d1:sha256:wf",
				InputRef:       "",
				Runtimes:       []ResolvedRuntime{},
			}),
		},
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if rs.Input != nil {
		t.Errorf("RunState.Input = %+v, want nil for no-input workflow", rs.Input)
	}
}

func TestFold_RunResumedBumpsEpoch(t *testing.T) {
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: mustMarshal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Type: EventRunResumed,
			Data: mustMarshal(t, RunResumedData{Epoch: 2})},
		{Seq: 3, TS: fixedTS, Type: EventRunResumed,
			Data: mustMarshal(t, RunResumedData{Epoch: 3})},
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if rs.Epoch != 3 {
		t.Errorf("Epoch after two resumes = %d, want 3", rs.Epoch)
	}
}

func TestFold_NodeCompleted(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	outputsRef, err := blobs.Put([]byte(`{"web_exploitable":true,"has_source":false}`))
	if err != nil {
		t.Fatalf("seed outputs blob: %v", err)
	}
	exit := 0
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: mustMarshal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{
			Seq: 2, TS: fixedTS, Path: "triage", Type: EventNodeCompleted,
			Data: mustMarshal(t, NodeCompletedData{
				Outcome:    "ok",
				ExitCode:   &exit,
				OutputsRef: outputsRef,
				Files:      map[string]string{"/out/a": "awf-d1:sha256:file"},
			}),
		},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got, ok := rs.Completed["triage"]
	if !ok {
		t.Fatalf("Completed[\"triage\"] missing; got %+v", rs.Completed)
	}
	if got.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want ok", got.Outcome)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", got.ExitCode)
	}
	if got.OutputsRef != outputsRef {
		t.Errorf("OutputsRef = %q", got.OutputsRef)
	}
	if got.Outputs["web_exploitable"] != true {
		t.Errorf("Outputs.web_exploitable = %v, want true", got.Outputs["web_exploitable"])
	}
	if got.Files["/out/a"] != "awf-d1:sha256:file" {
		t.Errorf("Files = %+v", got.Files)
	}
}

func TestFold_BranchTaken(t *testing.T) {
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: mustMarshal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Path: "if[0]", Type: EventBranchTaken,
			Data: mustMarshal(t, BranchTakenData{Which: "then"})},
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if rs.Branches["if[0]"] != "then" {
		t.Errorf("Branches[if[0]] = %q, want then", rs.Branches["if[0]"])
	}
}

func TestFold_LoopIterMax(t *testing.T) {
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: mustMarshal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Path: "loop[0]", Type: EventLoopIter,
			Data: mustMarshal(t, LoopIterData{N: 1})},
		{Seq: 3, TS: fixedTS, Path: "loop[0]", Type: EventLoopIter,
			Data: mustMarshal(t, LoopIterData{N: 2})},
		{Seq: 4, TS: fixedTS, Path: "loop[0]", Type: EventLoopIter,
			Data: mustMarshal(t, LoopIterData{N: 3})},
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if rs.LoopIters["loop[0]"] != 3 {
		t.Errorf("LoopIters[loop[0]] = %d, want 3", rs.LoopIters["loop[0]"])
	}
}

func TestFold_IgnoresUnknownEventType(t *testing.T) {
	// Future event types (slice 2.4 retry.attempt; 2.5 node.started / node.failed /
	// run.finished; later phases signal.received / map.item / agent.event / …) are
	// not in the 2.1 fold's dispatch table. They land in the default case and don't
	// touch RunState. Use raw string Type values to pin that no constant is needed.
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: mustMarshal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Path: "triage", Type: "node.started", Data: json.RawMessage(`{"attempt":1}`)},
		{Seq: 3, TS: fixedTS, Path: "triage", Type: "retry.attempt", Data: json.RawMessage(`{"n":1}`)},
		{Seq: 4, TS: fixedTS, Type: "future.event", Data: json.RawMessage(`{"anything":true}`)},
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(rs.Completed) != 0 {
		t.Errorf("Completed should be empty (no commit events), got %+v", rs.Completed)
	}
}

func TestFold_FirstEventMustBeRunStarted(t *testing.T) {
	// Non-empty event list whose first event isn't run.started → corruption or a
	// writer bug. Surface it instead of producing a half-populated RunState.
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Path: "x", Type: EventNodeCompleted,
			Data: mustMarshal(t, NodeCompletedData{Outcome: "ok"})},
	}
	if _, err := Fold(events, state.NewInMemoryBlobs()); err == nil {
		t.Errorf("Fold with non-run.started first event should error, got nil")
	}
}

func TestFold_DuplicateRunStartedIsError(t *testing.T) {
	// A second run.started in one log can only come from corruption or a writer bug
	// (the engine never emits it twice). Erroring catches the bug early instead of
	// silently overwriting RunID / WorkflowDigest.
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: mustMarshal(t, RunStartedData{RunID: "first", WorkflowDigest: "wf1", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Type: EventRunStarted,
			Data: mustMarshal(t, RunStartedData{RunID: "second", WorkflowDigest: "wf2", Runtimes: []ResolvedRuntime{}})},
	}
	if _, err := Fold(events, state.NewInMemoryBlobs()); err == nil {
		t.Errorf("Fold with duplicate run.started should error, got nil")
	}
}

func TestFold_UnknownOutcomeIsError(t *testing.T) {
	// node.completed with an outcome string outside the three known values is
	// corruption — the engine only writes "ok" on commit. ParseOutcome catches it.
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: mustMarshal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Path: "step", Type: EventNodeCompleted,
			Data: mustMarshal(t, NodeCompletedData{Outcome: "fubar"})},
	}
	if _, err := Fold(events, state.NewInMemoryBlobs()); err == nil {
		t.Errorf("Fold with unknown outcome should error, got nil")
	}
}

func TestFold_GoldenEquivalence(t *testing.T) {
	// A multi-event log → RunState that matches a hand-built RunState field-for-field.
	// This is the spec exit criterion ("log → RunState fold golden-equivalent to a
	// hand-built RunState"). Pin a small but representative scenario:
	//   - run.started + an `input` payload
	//   - one committed step (triage, with outputs + one output_file)
	//   - one branch.taken (if[0] → then)
	//   - one committed step inside the chosen branch (approve)
	//   - one loop.iter @ N=2 on a top-level loop
	blobs := state.NewInMemoryBlobs()
	inputRef, err := blobs.Put([]byte(`{"cve_id":"CVE-2024-0001"}`))
	if err != nil {
		t.Fatalf("seed input: %v", err)
	}
	triageOutRef, err := blobs.Put([]byte(`{"web_exploitable":true,"has_source":true}`))
	if err != nil {
		t.Fatalf("seed triage outputs: %v", err)
	}
	approveOutRef, err := blobs.Put([]byte(`{"approved":true}`))
	if err != nil {
		t.Fatalf("seed approve outputs: %v", err)
	}
	exit0 := 0

	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted, Data: mustMarshal(t, RunStartedData{
			RunID: "run-a", WorkflowDigest: "awf-d1:sha256:wf",
			InputRef: inputRef, Runtimes: []ResolvedRuntime{},
		})},
		{Seq: 2, TS: fixedTS, Path: "triage", Type: EventNodeCompleted, Data: mustMarshal(t, NodeCompletedData{
			Outcome: "ok", ExitCode: &exit0, OutputsRef: triageOutRef,
			Files: map[string]string{"/out/triage.json": "awf-d1:sha256:filea"},
		})},
		{Seq: 3, TS: fixedTS, Path: "if[1]", Type: EventBranchTaken, Data: mustMarshal(t, BranchTakenData{Which: "then"})},
		{Seq: 4, TS: fixedTS, Path: "if[1].then.approve", Type: EventNodeCompleted, Data: mustMarshal(t, NodeCompletedData{
			Outcome: "ok", ExitCode: &exit0, OutputsRef: approveOutRef,
		})},
		{Seq: 5, TS: fixedTS, Path: "loop[2]", Type: EventLoopIter, Data: mustMarshal(t, LoopIterData{N: 2})},
	}

	got, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}

	// Inline per-field assertions, matching the project's existing test style
	// (state/fake_test.go's TestInMemoryLogSmoke).
	if got.RunID != "run-a" {
		t.Errorf("RunID: got %q, want %q", got.RunID, "run-a")
	}
	if got.WorkflowDigest != "awf-d1:sha256:wf" {
		t.Errorf("WorkflowDigest: got %q", got.WorkflowDigest)
	}
	if got.Epoch != 1 {
		t.Errorf("Epoch: got %d, want 1", got.Epoch)
	}
	if got.Input["cve_id"] != "CVE-2024-0001" {
		t.Errorf("Input[cve_id]: got %v", got.Input["cve_id"])
	}

	// triage
	triage, ok := got.Completed["triage"]
	if !ok {
		t.Fatalf("Completed[\"triage\"] missing")
	}
	if triage.Outcome != OutcomeOK {
		t.Errorf("triage.Outcome: got %q, want ok", triage.Outcome)
	}
	if triage.ExitCode == nil || *triage.ExitCode != 0 {
		t.Errorf("triage.ExitCode: got %v, want 0", triage.ExitCode)
	}
	if triage.OutputsRef != triageOutRef {
		t.Errorf("triage.OutputsRef: got %q", triage.OutputsRef)
	}
	if triage.Outputs["web_exploitable"] != true || triage.Outputs["has_source"] != true {
		t.Errorf("triage.Outputs: got %+v", triage.Outputs)
	}
	if triage.Files["/out/triage.json"] != "awf-d1:sha256:filea" {
		t.Errorf("triage.Files: got %+v", triage.Files)
	}

	// approve (no Files; has Outputs)
	approve, ok := got.Completed["if[1].then.approve"]
	if !ok {
		t.Fatalf("Completed[\"if[1].then.approve\"] missing")
	}
	if approve.Outcome != OutcomeOK {
		t.Errorf("approve.Outcome: got %q, want ok", approve.Outcome)
	}
	if approve.OutputsRef != approveOutRef {
		t.Errorf("approve.OutputsRef: got %q", approve.OutputsRef)
	}
	if approve.Outputs["approved"] != true {
		t.Errorf("approve.Outputs[approved]: got %v", approve.Outputs["approved"])
	}
	if len(approve.Files) != 0 {
		t.Errorf("approve.Files: got %+v, want empty", approve.Files)
	}

	if got.Branches["if[1]"] != "then" {
		t.Errorf("Branches[if[1]]: got %q", got.Branches["if[1]"])
	}
	if got.LoopIters["loop[2]"] != 2 {
		t.Errorf("LoopIters[loop[2]]: got %d", got.LoopIters["loop[2]"])
	}

	// Negative checks — no Completed entries should exist that weren't in the events.
	if len(got.Completed) != 2 {
		t.Errorf("Completed: got %d entries, want 2", len(got.Completed))
	}
}

func TestFold_MalformedDataIsError(t *testing.T) {
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted, Data: json.RawMessage(`not json`)},
	}
	if _, err := Fold(events, state.NewInMemoryBlobs()); err == nil {
		t.Errorf("Fold with malformed Data should error, got nil")
	}
}

func TestFold_MissingInputBlobIsError(t *testing.T) {
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted, Data: mustMarshal(t, RunStartedData{
			RunID: "x", WorkflowDigest: "y",
			// Well-formed ref (correct prefix + 64 hex chars), but the blob was never Put —
			// state.InMemoryBlobs.Get returns wrapped fs.ErrNotExist, which Fold surfaces.
			InputRef: "awf-d1:sha256:" + strings.Repeat("ab", 32),
			Runtimes: []ResolvedRuntime{},
		})},
	}
	if _, err := Fold(events, state.NewInMemoryBlobs()); err == nil {
		t.Errorf("Fold with missing input blob should error, got nil")
	}
}
