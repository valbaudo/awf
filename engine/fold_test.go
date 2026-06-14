package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/state"
)

// fixedTS is a deterministic timestamp used in test events. The fold doesn't read TS
// (it only carries metadata), but events need a non-zero TS to round-trip cleanly.
var fixedTS = time.Unix(1700000000, 0).UTC()

// marshalOrFatal calls t.Fatalf on marshal error — test helper for cleaner table cases.
// (Named with the "OrFatal" suffix rather than "must" prefix to make the failure mode
// honest: idiomatic Go uses "Must*" for panic-on-error, e.g. regexp.MustCompile.)
func marshalOrFatal(t *testing.T, v any) json.RawMessage {
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
			Data: marshalOrFatal(t, RunStartedData{
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

func TestFold_RunStartedAssetsDoNotDereferenceBlobs(t *testing.T) {
	missingRef := "awf-d1:sha256:" + strings.Repeat("b", 64)
	events := []state.Event{
		{
			Seq: 1, Epoch: 0, TS: fixedTS,
			Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{
				RunID:          "deadbeef",
				WorkflowDigest: "awf-d1:sha256:wf",
				Assets: map[string]RunStartedAsset{
					"input": {
						DeclaredPath: "asset.txt",
						Files: []RunStartedAssetFile{{
							Path: ".", Ref: missingRef, Size: 5, SHA256: strings.Repeat("0", 64),
						}},
					},
				},
			}),
		},
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold dereferenced asset ref %q: %v", missingRef, err)
	}
	if rs.RunID != "deadbeef" || rs.WorkflowDigest != "awf-d1:sha256:wf" {
		t.Fatalf("RunState = %+v", rs)
	}
	started, err := RunStartedDataFromEvents(events)
	if err != nil {
		t.Fatalf("RunStartedDataFromEvents: %v", err)
	}
	if got := started.Assets["input"].Files[0].Ref; got != missingRef {
		t.Fatalf("recorded asset ref = %q, want %q", got, missingRef)
	}
}

func TestFold_RunStartedWithoutInput(t *testing.T) {
	// A workflow without an `input:` declaration emits run.started with InputRef="".
	// The fold leaves RunState.Input nil (NOT empty-map).
	events := []state.Event{
		{
			Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{
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

func TestFoldCallStartedMaterializesInputAndRuntimes(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	inputRef, err := blobs.Put([]byte(`{"task":"audit","deep":true}`))
	if err != nil {
		t.Fatalf("seed call input: %v", err)
	}
	runtimes := []ResolvedRuntime{
		{Ref: "anthropic/claude-code", Version: "2.1.118", Container: "lab"},
		{Ref: "openai/codex", Version: "0.31.0"},
	}
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Path: "call.review", Type: EventCallStarted,
			Data: marshalOrFatal(t, CallStartedData{InputRef: inputRef, Runtimes: runtimes})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got, ok := rs.LookupCallStarted("call.review")
	if !ok {
		t.Fatal("LookupCallStarted(call.review): ok=false")
	}
	if got.InputRef != inputRef {
		t.Errorf("InputRef = %q, want %q", got.InputRef, inputRef)
	}
	if got.Input["task"] != "audit" || got.Input["deep"] != true {
		t.Errorf("Input = %+v, want task=audit deep=true", got.Input)
	}
	if len(got.Runtimes) != len(runtimes) {
		t.Fatalf("len(Runtimes) = %d, want %d", len(got.Runtimes), len(runtimes))
	}
	for i := range runtimes {
		if got.Runtimes[i] != runtimes[i] {
			t.Errorf("Runtimes[%d] = %+v, want %+v", i, got.Runtimes[i], runtimes[i])
		}
	}
}

func TestFoldCallStartedRecordsInputFiles(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	inputRef, err := blobs.Put([]byte(`{"task":"audit"}`))
	if err != nil {
		t.Fatalf("seed call input: %v", err)
	}
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Path: "scan", Type: EventCallStarted,
			Data: marshalOrFatal(t, CallStartedData{
				InputRef:   inputRef,
				InputFiles: map[string]string{"report": "sha256:report"},
			})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got, ok := rs.LookupCallStarted("scan")
	if !ok {
		t.Fatal("LookupCallStarted(scan): ok=false")
	}
	if got.InputFiles["report"] != "sha256:report" {
		t.Errorf("InputFiles[report] = %q, want sha256:report", got.InputFiles["report"])
	}
}

func TestFoldCallStartedMissingInputBlobIsError(t *testing.T) {
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Path: "call.review", Type: EventCallStarted,
			Data: marshalOrFatal(t, CallStartedData{
				InputRef: "awf-d1:sha256:" + strings.Repeat("ab", 32),
			})},
	}
	if _, err := Fold(events, state.NewInMemoryBlobs()); err == nil {
		t.Errorf("Fold with missing call input blob should error, got nil")
	}
}

func TestFoldDuplicateCallStartedFails(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	inputRef, err := blobs.Put([]byte(`{"task":"audit"}`))
	if err != nil {
		t.Fatalf("seed call input: %v", err)
	}
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Path: "call.review", Type: EventCallStarted,
			Data: marshalOrFatal(t, CallStartedData{InputRef: inputRef})},
		{Seq: 3, TS: fixedTS, Path: "call.review", Type: EventCallStarted,
			Data: marshalOrFatal(t, CallStartedData{
				InputRef: "awf-d1:sha256:" + strings.Repeat("cd", 32),
			})},
	}
	_, err = Fold(events, blobs)
	if err == nil {
		t.Fatal("Fold accepted duplicate call.started: want error")
	}
	if !strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), EventCallStarted) {
		t.Errorf("err = %v, want duplicate call.started", err)
	}
}

func TestFold_RunResumedBumpsEpoch(t *testing.T) {
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Type: EventRunResumed,
			Data: marshalOrFatal(t, RunResumedData{Epoch: 2})},
		{Seq: 3, TS: fixedTS, Type: EventRunResumed,
			Data: marshalOrFatal(t, RunResumedData{Epoch: 3})},
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
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{
			Seq: 2, TS: fixedTS, Path: "triage", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{
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

func TestFold_NodeCompletedWithStdoutRef(t *testing.T) {
	// Slice 2.4 extension: NodeCompletedData.StdoutRef → blobs.Get → nr.Stdout.
	// Same atomicity invariant as OutputsRef: a committed node referencing a
	// missing stdout blob is a §8 violation (covered by the missing-blob test
	// below; here we pin the happy path).
	blobs := state.NewInMemoryBlobs()
	stdoutRef, err := blobs.Put([]byte("hello"))
	if err != nil {
		t.Fatalf("seed stdout blob: %v", err)
	}
	exit := 0
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{
			Seq: 2, TS: fixedTS, Path: "triage", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{
				Outcome:   "ok",
				ExitCode:  &exit,
				StdoutRef: stdoutRef,
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
	if got.StdoutRef != stdoutRef {
		t.Errorf("StdoutRef = %q, want %q", got.StdoutRef, stdoutRef)
	}
	if string(got.Stdout) != "hello" {
		t.Errorf("Stdout = %q, want %q", got.Stdout, "hello")
	}
}

func TestFold_MissingStdoutBlobIsError(t *testing.T) {
	// Symmetric to TestFold_MissingOutputsBlobIsError: a node.completed with a
	// well-formed but absent StdoutRef → fold error. Same §8 atomic-commit
	// invariant: a committed node referencing a missing artifact means the
	// commit boundary protocol was broken and resume must not proceed.
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Path: "step", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{
				Outcome:   "ok",
				StdoutRef: "awf-d1:sha256:" + strings.Repeat("ef", 32),
			})},
	}
	if _, err := Fold(events, state.NewInMemoryBlobs()); err == nil {
		t.Errorf("Fold with missing stdout blob should error, got nil")
	}
}

func TestFold_BranchTaken(t *testing.T) {
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Path: "if[0]", Type: EventBranchTaken,
			Data: marshalOrFatal(t, BranchTakenData{Which: "then"})},
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
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Path: "loop[0]", Type: EventLoopIter,
			Data: marshalOrFatal(t, LoopIterData{N: 1})},
		{Seq: 3, TS: fixedTS, Path: "loop[0]", Type: EventLoopIter,
			Data: marshalOrFatal(t, LoopIterData{N: 2})},
		{Seq: 4, TS: fixedTS, Path: "loop[0]", Type: EventLoopIter,
			Data: marshalOrFatal(t, LoopIterData{N: 3})},
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
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
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
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok"})},
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
			Data: marshalOrFatal(t, RunStartedData{RunID: "first", WorkflowDigest: "wf1", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "second", WorkflowDigest: "wf2", Runtimes: []ResolvedRuntime{}})},
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
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Path: "step", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "fubar"})},
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
		{Seq: 1, TS: fixedTS, Type: EventRunStarted, Data: marshalOrFatal(t, RunStartedData{
			RunID: "run-a", WorkflowDigest: "awf-d1:sha256:wf",
			InputRef: inputRef, Runtimes: []ResolvedRuntime{},
		})},
		{Seq: 2, TS: fixedTS, Path: "triage", Type: EventNodeCompleted, Data: marshalOrFatal(t, NodeCompletedData{
			Outcome: "ok", ExitCode: &exit0, OutputsRef: triageOutRef,
			Files: map[string]string{"/out/triage.json": "awf-d1:sha256:filea"},
		})},
		{Seq: 3, TS: fixedTS, Path: "if[1]", Type: EventBranchTaken, Data: marshalOrFatal(t, BranchTakenData{Which: "then"})},
		{Seq: 4, TS: fixedTS, Path: "if[1].then.approve", Type: EventNodeCompleted, Data: marshalOrFatal(t, NodeCompletedData{
			Outcome: "ok", ExitCode: &exit0, OutputsRef: approveOutRef,
		})},
		{Seq: 5, TS: fixedTS, Path: "loop[2]", Type: EventLoopIter, Data: marshalOrFatal(t, LoopIterData{N: 2})},
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
		{Seq: 1, TS: fixedTS, Type: EventRunStarted, Data: marshalOrFatal(t, RunStartedData{
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

func TestFold_MissingOutputsBlobIsError(t *testing.T) {
	// Symmetric to TestFold_MissingInputBlobIsError: node.completed with a
	// well-formed but absent OutputsRef → fold error. This is the §8 atomic-commit
	// invariant — a committed node referencing a missing artifact means the commit
	// boundary protocol was broken (or the log was tampered with), and resume must
	// not proceed.
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y", Runtimes: []ResolvedRuntime{}})},
		{Seq: 2, TS: fixedTS, Path: "step", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{
				Outcome: "ok",
				// Well-formed ref but blob was never Put — state.InMemoryBlobs.Get
				// returns wrapped fs.ErrNotExist, which Fold surfaces.
				OutputsRef: "awf-d1:sha256:" + strings.Repeat("cd", 32),
			})},
	}
	if _, err := Fold(events, state.NewInMemoryBlobs()); err == nil {
		t.Errorf("Fold with missing outputs blob should error, got nil")
	}
}

// TestFold_NodeCompletedWithNonOkOutcomeIsError pins that even a syntactically-valid
// non-ok outcome in node.completed is a fold error — the spec §8 commit invariant
// says only ok-steps commit, so ParseOutcome accepting retryable_failure /
// permanent_failure here would be too wide. Closes the gap the bare ParseOutcome call
// would otherwise leave.
func TestFold_NodeCompletedWithNonOkOutcomeIsError(t *testing.T) {
	for _, oc := range []string{"retryable_failure", "permanent_failure"} {
		t.Run(oc, func(t *testing.T) {
			events := []state.Event{
				{Seq: 1, TS: fixedTS, Type: EventRunStarted,
					Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
				{Seq: 2, TS: fixedTS, Path: "step", Type: EventNodeCompleted,
					Data: marshalOrFatal(t, NodeCompletedData{Outcome: oc})},
			}
			if _, err := Fold(events, state.NewInMemoryBlobs()); err == nil {
				t.Errorf("Fold with node.completed outcome=%q should error, got nil", oc)
			}
		})
	}
}

func TestFold_NodeCompletedRejectedFails(t *testing.T) {
	// Spec §8 + CLAUDE.md commit invariant: only ok-steps commit.
	// A node.completed event with outcome:"rejected" is corruption — a gate
	// rejection never commits as node.completed (the gate.attempt event with
	// attempt_outcome:"attempt_rejected" + the OutcomeRejected return from the
	// gate handler are how rejections propagate, NOT node.completed).
	// Fold MUST reject this event class to surface corruption immediately.
	blobs := state.NewInMemoryBlobs()
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Type: EventNodeCompleted, Path: "gate[0]",
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "rejected"})},
	}
	_, err := Fold(events, blobs)
	if err == nil {
		t.Fatal("Fold accepted node.completed{outcome:\"rejected\"}: want error per spec §8")
	}
	if !strings.Contains(err.Error(), "only") || !strings.Contains(err.Error(), "commits") {
		t.Errorf("Fold error = %q, want mention of \"only %q commits\" (spec §8 wording)", err, OutcomeOK)
	}
}

// TestFold_MalformedDataPerEventType covers JSON-unmarshal failures on every dispatch
// case the fold handles. The pre-existing TestFold_MalformedDataIsError only hits the
// run.started branch — this table covers run.resumed / node.completed / branch.taken
// / loop.iter so a regression in any single unmarshal call surfaces directly.
func TestFold_MalformedDataPerEventType(t *testing.T) {
	// Seed a valid run.started so the per-type bad-Data event isn't the first event
	// (which would error for a different reason).
	runStarted := state.Event{
		Seq: 1, TS: fixedTS, Type: EventRunStarted,
		Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"}),
	}
	cases := []struct {
		name      string
		eventType string
	}{
		{"run.resumed", EventRunResumed},
		{"node.completed", EventNodeCompleted},
		{"branch.taken", EventBranchTaken},
		{"loop.iter", EventLoopIter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events := []state.Event{
				runStarted,
				{Seq: 2, TS: fixedTS, Path: "p", Type: c.eventType,
					Data: json.RawMessage(`not json`)},
			}
			if _, err := Fold(events, state.NewInMemoryBlobs()); err == nil {
				t.Errorf("Fold with malformed Data on %s should error, got nil", c.eventType)
			}
		})
	}
}

func TestFold_GateAttemptPopulatesAttempts(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	verdictRef, err := blobs.Put([]byte(`{"verified":false,"feedback":"missing X"}`))
	if err != nil {
		t.Fatalf("seed verdict blob: %v", err)
	}
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Type: EventGateAttempt, Path: "gate[0]",
			Data: marshalOrFatal(t, GateAttemptData{N: 1, AttemptOutcome: AttemptRejected, VerdictRef: verdictRef})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	attempts := rs.GateAttempts["gate[0]"]
	if len(attempts) != 1 {
		t.Fatalf("attempts len = %d, want 1", len(attempts))
	}
	if attempts[0].N != 1 || attempts[0].AttemptOutcome != AttemptRejected {
		t.Errorf("attempts[0] = %+v, want N=1 AttemptRejected", attempts[0])
	}
	if attempts[0].Verdict["verified"] != false || attempts[0].Verdict["feedback"] != "missing X" {
		t.Errorf("attempts[0].Verdict = %+v, want verified=false feedback=\"missing X\"", attempts[0].Verdict)
	}
}

func TestFold_GateAttemptMultipleAttemptsOrdered(t *testing.T) {
	// Two attempts on the SAME gate path. Order MUST be preserved (oldest first)
	// so resolveEvaluate's "latest verdict = attempts[len-1]" semantics work.
	blobs := state.NewInMemoryBlobs()
	ref1, _ := blobs.Put([]byte(`{"verified":false}`))
	ref2, _ := blobs.Put([]byte(`{"verified":true}`))
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Type: EventGateAttempt, Path: "gate[0]",
			Data: marshalOrFatal(t, GateAttemptData{N: 1, AttemptOutcome: AttemptRejected, VerdictRef: ref1})},
		{Seq: 3, TS: fixedTS, Type: EventGateAttempt, Path: "gate[0]",
			Data: marshalOrFatal(t, GateAttemptData{N: 2, AttemptOutcome: AttemptPassed, VerdictRef: ref2})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := rs.GateAttempts["gate[0]"]
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].N != 1 || got[1].N != 2 {
		t.Errorf("order: got Ns %d,%d; want 1,2", got[0].N, got[1].N)
	}
	if got[1].AttemptOutcome != AttemptPassed {
		t.Errorf("latest AttemptOutcome = %q, want %q", got[1].AttemptOutcome, AttemptPassed)
	}
}

func TestFold_GateAttemptUnknownVerdictRefErrors(t *testing.T) {
	// Phase 3 design §D + spec §8: the verdict_ref is the CAS pointer to the
	// last evaluator step's typed outputs, stored via the same Blobs interface
	// as node.completed's OutputsRef. A gate.attempt event referencing a
	// missing blob is corruption (the §8 commit-atomicity invariant was
	// violated by the writer). Fold MUST surface this.
	blobs := state.NewInMemoryBlobs() // empty — verdict_ref will miss
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Type: EventGateAttempt, Path: "gate[0]",
			Data: marshalOrFatal(t, GateAttemptData{N: 1, AttemptOutcome: AttemptPassed,
				VerdictRef: "awf-blob-v1:sha256:nonexistent"})},
	}
	_, err := Fold(events, blobs)
	if err == nil {
		t.Fatal("Fold accepted gate.attempt with missing verdict_ref: want error")
	}
	if !strings.Contains(err.Error(), "verdict") {
		t.Errorf("err = %v, want mention of \"verdict\"", err)
	}
}

func TestFold_MapItemPopulatesItems(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Type: EventMapItem, Path: "map[0]",
			Data: marshalOrFatal(t, MapItemData{N: 0, Status: ItemPassed})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	items := rs.MapItems["map[0]"]
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	if items[0].N != 0 || items[0].Status != ItemPassed {
		t.Errorf("items[0] = %+v, want N=0 Status=item_passed", items[0])
	}
	// Design Q3: ItemValue is NOT in the wire format; Fold leaves it nil.
	// The runtime executor fills it via UpdateMapItemValue on re-entry.
	if items[0].ItemValue != nil {
		t.Errorf("items[0].ItemValue = %v, want nil (slice 3.4 Design Q3 contract)", items[0].ItemValue)
	}
}

func TestFold_MapItemMultipleItemsArrivalOrdered(t *testing.T) {
	// Items may commit OUT of N-order (concurrent goroutines). Fold preserves
	// ARRIVAL order in the slice. The handler walks N-by-N when it needs to
	// check completion; arrival order is for fidelity to the log.
	blobs := state.NewInMemoryBlobs()
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		// N=2 arrives BEFORE N=0 (concurrent dispatch).
		{Seq: 2, TS: fixedTS, Type: EventMapItem, Path: "map[0]",
			Data: marshalOrFatal(t, MapItemData{N: 2, Status: ItemPassed})},
		{Seq: 3, TS: fixedTS, Type: EventMapItem, Path: "map[0]",
			Data: marshalOrFatal(t, MapItemData{N: 0, Status: ItemFailed})},
		{Seq: 4, TS: fixedTS, Type: EventMapItem, Path: "map[0]",
			Data: marshalOrFatal(t, MapItemData{N: 1, Status: ItemPassed})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := rs.MapItems["map[0]"]
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Arrival order pinned.
	if got[0].N != 2 || got[1].N != 0 || got[2].N != 1 {
		t.Errorf("arrival order broken: got Ns %d,%d,%d; want 2,0,1",
			got[0].N, got[1].N, got[2].N)
	}
}

func TestFold_MapItemMalformedDataErrors(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Type: EventMapItem, Path: "map[0]",
			Data: []byte(`{not valid json`)},
	}
	_, err := Fold(events, blobs)
	if err == nil {
		t.Fatal("Fold accepted malformed map.item: want error")
	}
	if !strings.Contains(err.Error(), "map.item") {
		t.Errorf("err = %v, want mention of \"map.item\"", err)
	}
}

func TestFold_MapItemNoBlobsAccess(t *testing.T) {
	// Defense-in-depth: unlike gate.attempt (which dereferences VerdictRef via
	// Blobs.Get and can fail with "verdict_ref not in blobs"), map.item has NO
	// CAS reference. The Fold of a map.item event MUST NOT call blobs.Get —
	// verified by passing a nil-ish blobs and asserting Fold succeeds.
	// (We use a real InMemoryBlobs but assert no entries get added by Fold.)
	blobs := state.NewInMemoryBlobs()
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Type: EventMapItem, Path: "map[0]",
			Data: marshalOrFatal(t, MapItemData{N: 0, Status: ItemPassed})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if got := rs.MapItems["map[0]"]; len(got) != 1 {
		t.Errorf("MapItems = %+v, want 1 entry", got)
	}
}

func TestFold_SignalReceivedPopulatesSignals(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	payloadRef, err := blobs.Put([]byte(`{"approved":true}`))
	if err != nil {
		t.Fatalf("Put payload: %v", err)
	}
	startedData, _ := json.Marshal(RunStartedData{RunID: "r", WorkflowDigest: "d"})
	sigData, _ := json.Marshal(SignalReceivedData{
		Name: "human_review", Seq: 1, PayloadRef: payloadRef,
	})
	events := []state.Event{
		{Type: EventRunStarted, Data: startedData, Seq: 1},
		{Type: EventSignalReceived, Path: "step.approve", Data: sigData, Seq: 2},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	// Signals queue gets {Seq, PayloadRef} (REFS ONLY — no Payload field per C6).
	sigs := rs.LookupSignals("human_review")
	if len(sigs) != 1 {
		t.Fatalf("Signals[human_review] len = %d, want 1", len(sigs))
	}
	if sigs[0].Seq != 1 || sigs[0].PayloadRef != payloadRef {
		t.Errorf("got %+v, want {Seq:1, PayloadRef:%s}", sigs[0], payloadRef)
	}
	// SignalReceivedAt[path] also populated.
	entry, ok := rs.LookupSignalReceivedAt("step.approve")
	if !ok {
		t.Fatal("LookupSignalReceivedAt: ok=false")
	}
	if entry.Seq != 1 || entry.PayloadRef != payloadRef {
		t.Errorf("entry = %+v", entry)
	}
}

func TestFold_SignalReceivedNonObjectPayload(t *testing.T) {
	// C6 regression test: payload is a JSON array (unschema'd signal). Fold
	// must NOT attempt json.Unmarshal into map[string]any; it stores refs only.
	blobs := state.NewInMemoryBlobs()
	payloadRef, err := blobs.Put([]byte(`[1,2,3]`))
	if err != nil {
		t.Fatalf("Put payload: %v", err)
	}
	startedData, _ := json.Marshal(RunStartedData{RunID: "r", WorkflowDigest: "d"})
	sigData, _ := json.Marshal(SignalReceivedData{
		Name: "ack", Seq: 1, PayloadRef: payloadRef,
	})
	events := []state.Event{
		{Type: EventRunStarted, Data: startedData, Seq: 1},
		{Type: EventSignalReceived, Path: "step.ack", Data: sigData, Seq: 2},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold errored on non-object payload (C6 regression): %v", err)
	}
	entry, ok := rs.LookupSignalReceivedAt("step.ack")
	if !ok || entry.PayloadRef != payloadRef {
		t.Errorf("non-object payload: SignalReceivedAt = %+v ok=%v", entry, ok)
	}
}

func TestFold_SignalReceivedNoPayloadRef(t *testing.T) {
	// signal with no payload (await step with no output_schema) → PayloadRef
	// empty → SignalReceivedAt entry still populated (refs only; empty ref OK).
	blobs := state.NewInMemoryBlobs()
	startedData, _ := json.Marshal(RunStartedData{RunID: "r", WorkflowDigest: "d"})
	sigData, _ := json.Marshal(SignalReceivedData{Name: "tick", Seq: 1})
	events := []state.Event{
		{Type: EventRunStarted, Data: startedData, Seq: 1},
		{Type: EventSignalReceived, Path: "step.tick", Data: sigData, Seq: 2},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	sigs := rs.LookupSignals("tick")
	if len(sigs) != 1 || sigs[0].PayloadRef != "" {
		t.Errorf("empty-PayloadRef signal: got %+v", sigs)
	}
}

func TestFold_RunPausedIsIgnored(t *testing.T) {
	// C7 regression test: Fold IGNORES run.paused events (default arm).
	// rs.Paused stays nil; runtime-only flag (set by live pollControls).
	startedData, _ := json.Marshal(RunStartedData{RunID: "r", WorkflowDigest: "d"})
	pausedData, _ := json.Marshal(RunPausedData{NodePath: "step.x", Reason: "test"})
	events := []state.Event{
		{Type: EventRunStarted, Data: startedData, Seq: 1},
		{Type: EventRunPaused, Data: pausedData, Seq: 2},
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if pm := rs.LookupPaused(); pm != nil {
		t.Errorf("Fold populated rs.Paused from run.paused (C7 regression): got %+v, want nil", pm)
	}
}

func TestFold_RunCancelledIsTerminal(t *testing.T) {
	startedData, _ := json.Marshal(RunStartedData{RunID: "r", WorkflowDigest: "d"})
	cancelledData, _ := json.Marshal(RunCancelledData{Reason: "test"})
	events := []state.Event{
		{Type: EventRunStarted, Data: startedData, Seq: 1},
		{Type: EventRunCancelled, Data: cancelledData, Seq: 2},
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if !rs.IsCancelled() {
		t.Errorf("IsCancelled = false after run.cancelled, want true")
	}
}

func TestFoldTracksLatestSnapshotRefPerContainer(t *testing.T) {
	mk := func(path, ref, ctr string) state.Event {
		d, _ := json.Marshal(NodeCompletedData{Outcome: "ok", SnapshotRef: ref, Container: ctr})
		return state.Event{Type: EventNodeCompleted, Path: path, Data: d}
	}
	rsd, _ := json.Marshal(RunStartedData{RunID: "r"})
	events := []state.Event{
		{Type: EventRunStarted, Data: rsd},
		mk("a", "r1", "ws"), mk("b", "rx", "other"), mk("c", "r2", "ws"),
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if rs.SnapshotRefs["ws"] != "r2" || rs.SnapshotRefs["other"] != "rx" {
		t.Errorf("SnapshotRefs = %v, want {ws:r2, other:rx}", rs.SnapshotRefs)
	}
}

// TestFold_Golden_Sequential — a flat 3-step sequential workflow with no branches or
// loops. Verifies the most common shape — a linear pipeline — folds correctly with
// Completed entries keyed by step id.
func TestFold_Golden_Sequential(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	a, err := blobs.Put([]byte(`{"v":1}`))
	if err != nil {
		t.Fatalf("seed a: %v", err)
	}
	b, err := blobs.Put([]byte(`{"v":2}`))
	if err != nil {
		t.Fatalf("seed b: %v", err)
	}
	c, err := blobs.Put([]byte(`{"v":3}`))
	if err != nil {
		t.Fatalf("seed c: %v", err)
	}
	exit0 := 0
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "r", WorkflowDigest: "wf"})},
		{Seq: 2, TS: fixedTS, Path: "stepA", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok", ExitCode: &exit0, OutputsRef: a})},
		{Seq: 3, TS: fixedTS, Path: "stepB", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok", ExitCode: &exit0, OutputsRef: b})},
		{Seq: 4, TS: fixedTS, Path: "stepC", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok", ExitCode: &exit0, OutputsRef: c})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(rs.Completed) != 3 {
		t.Errorf("Completed: got %d, want 3", len(rs.Completed))
	}
	for _, p := range []string{"stepA", "stepB", "stepC"} {
		if _, ok := rs.Completed[p]; !ok {
			t.Errorf("Completed[%q] missing", p)
		}
	}
	if rs.Completed["stepA"].Outputs["v"] != float64(1) {
		t.Errorf("stepA.Outputs.v = %v, want 1", rs.Completed["stepA"].Outputs["v"])
	}
}

// TestFold_Golden_IfElseBranch — the if-false / else-branch case. Pins that fold
// records "else" in Branches and the else-branch step lands in Completed under the
// nested path.
func TestFold_Golden_IfElseBranch(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	out, err := blobs.Put([]byte(`{"ok":true}`))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	exit0 := 0
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "r", WorkflowDigest: "wf"})},
		{Seq: 2, TS: fixedTS, Path: "if[0]", Type: EventBranchTaken,
			Data: marshalOrFatal(t, BranchTakenData{Which: "else"})},
		{Seq: 3, TS: fixedTS, Path: "if[0].else.fallback", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok", ExitCode: &exit0, OutputsRef: out})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if rs.Branches["if[0]"] != "else" {
		t.Errorf("Branches[if[0]] = %q, want else", rs.Branches["if[0]"])
	}
	if _, ok := rs.Completed["if[0].else.fallback"]; !ok {
		t.Errorf("Completed[if[0].else.fallback] missing; got %+v", rs.Completed)
	}
}

// TestFold_Golden_LoopWithBodySteps — a loop with 3 iterations, each iteration's
// body step committing at a distinct iter-N path. Pins that LoopIters tracks the
// max N AND Completed has one entry per (iter, step) coordinate.
func TestFold_Golden_LoopWithBodySteps(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	out, err := blobs.Put([]byte(`{"n":1}`))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	exit0 := 0
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "r", WorkflowDigest: "wf"})},
		// iter 1: body step commits, then loop.iter
		{Seq: 2, TS: fixedTS, Path: "loop[0].body.iter-1.work", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok", ExitCode: &exit0, OutputsRef: out})},
		{Seq: 3, TS: fixedTS, Path: "loop[0]", Type: EventLoopIter,
			Data: marshalOrFatal(t, LoopIterData{N: 1})},
		// iter 2
		{Seq: 4, TS: fixedTS, Path: "loop[0].body.iter-2.work", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok", ExitCode: &exit0, OutputsRef: out})},
		{Seq: 5, TS: fixedTS, Path: "loop[0]", Type: EventLoopIter,
			Data: marshalOrFatal(t, LoopIterData{N: 2})},
		// iter 3
		{Seq: 6, TS: fixedTS, Path: "loop[0].body.iter-3.work", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok", ExitCode: &exit0, OutputsRef: out})},
		{Seq: 7, TS: fixedTS, Path: "loop[0]", Type: EventLoopIter,
			Data: marshalOrFatal(t, LoopIterData{N: 3})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if rs.LoopIters["loop[0]"] != 3 {
		t.Errorf("LoopIters[loop[0]] = %d, want 3", rs.LoopIters["loop[0]"])
	}
	if len(rs.Completed) != 3 {
		t.Errorf("Completed: got %d entries, want 3 (one per iter)", len(rs.Completed))
	}
	for _, p := range []string{
		"loop[0].body.iter-1.work",
		"loop[0].body.iter-2.work",
		"loop[0].body.iter-3.work",
	} {
		if _, ok := rs.Completed[p]; !ok {
			t.Errorf("Completed[%q] missing", p)
		}
	}
}

func TestFoldMaterializesTranscript(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	pair := agent.ThreadTurn{User: "u1", Assistant: "a1"}
	pb, err := json.Marshal(pair)
	if err != nil {
		t.Fatalf("marshal pair: %v", err)
	}
	ref, err := blobs.Put(pb)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	data, err := json.Marshal(NodeCompletedData{Outcome: "ok", TranscriptRef: ref})
	if err != nil {
		t.Fatalf("marshal NodeCompletedData: %v", err)
	}
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "r1", WorkflowDigest: "d"})},
		{Seq: 2, TS: fixedTS, Path: "turn1", Type: EventNodeCompleted, Data: data},
	}
	rs, foldErr := Fold(events, blobs)
	if foldErr != nil {
		t.Fatalf("Fold: %v", foldErr)
	}
	nr, ok := rs.Completed["turn1"]
	if !ok {
		t.Fatal("turn1 not in Completed")
	}
	if nr.Transcript.User != "u1" || nr.Transcript.Assistant != "a1" {
		t.Errorf("nr.Transcript = %+v, want {User:u1 Assistant:a1}", nr.Transcript)
	}
}

func TestFoldNoTranscriptRefIsZeroValue(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	data, err := json.Marshal(NodeCompletedData{Outcome: "ok"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "r1", WorkflowDigest: "d"})},
		{Seq: 2, TS: fixedTS, Path: "step", Type: EventNodeCompleted, Data: data},
	}
	rs, foldErr := Fold(events, blobs)
	if foldErr != nil {
		t.Fatalf("Fold: %v", foldErr)
	}
	if got := rs.Completed["step"].Transcript; got != (agent.ThreadTurn{}) {
		t.Errorf("Transcript = %+v, want zero value (no TranscriptRef)", got)
	}
}

func TestFoldMissingTranscriptBlobIsError(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	data, err := json.Marshal(NodeCompletedData{Outcome: "ok", TranscriptRef: "awf-d1:sha256:" + strings.Repeat("ff", 32)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "r1", WorkflowDigest: "d"})},
		{Seq: 2, TS: fixedTS, Path: "turn1", Type: EventNodeCompleted, Data: data},
	}
	if _, foldErr := Fold(events, blobs); foldErr == nil {
		t.Fatal("Fold should error on a node.completed referencing a missing transcript blob (spec §8 atomicity)")
	}
}

func TestFold_ReactRoundsAppend(t *testing.T) {
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted, Data: marshalOrFatal(t, RunStartedData{RunID: "x", WorkflowDigest: "y"})},
		{Seq: 2, TS: fixedTS, Type: EventReactRound, Path: "react[0]", Data: marshalOrFatal(t, ReactRoundData{N: 1})},
		{Seq: 3, TS: fixedTS, Type: EventReactRound, Path: "react[0]", Data: marshalOrFatal(t, ReactRoundData{N: 2})},
	}
	rs, err := Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	rounds := rs.ReactRounds["react[0]"]
	if len(rounds) != 2 || rounds[0].N != 1 || rounds[1].N != 2 {
		t.Fatalf("ReactRounds = %+v, want [{1} {2}]", rounds)
	}
}

func TestFoldNodeInvalidatedDeletes(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "r", WorkflowDigest: "d"})},
		{Seq: 2, TS: fixedTS, Path: "a", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok"})},
		{Seq: 3, TS: fixedTS, Path: "b", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok"})},
		{Seq: 4, TS: fixedTS, Type: EventNodeInvalidated,
			Data: marshalOrFatal(t, NodeInvalidatedData{Paths: []string{"b"}})},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if _, ok := rs.Completed["a"]; !ok {
		t.Fatal("a should remain committed")
	}
	if _, ok := rs.Completed["b"]; ok {
		t.Fatal("b should have been deleted by node.invalidated")
	}
}

func TestFoldNodeInvalidatedThenRecommitLastWins(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	events := []state.Event{
		{Seq: 1, TS: fixedTS, Type: EventRunStarted,
			Data: marshalOrFatal(t, RunStartedData{RunID: "r", WorkflowDigest: "d"})},
		{Seq: 2, TS: fixedTS, Path: "b", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok"})},
		{Seq: 3, TS: fixedTS, Type: EventNodeInvalidated,
			Data: marshalOrFatal(t, NodeInvalidatedData{Paths: []string{"b"}})},
		{Seq: 4, TS: fixedTS, Path: "b", Type: EventNodeCompleted,
			Data: marshalOrFatal(t, NodeCompletedData{Outcome: "ok"})}, // re-committed after invalidation
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if _, ok := rs.Completed["b"]; !ok {
		t.Fatal("re-committed b should be present (last event per path wins)")
	}
}
