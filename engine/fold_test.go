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
