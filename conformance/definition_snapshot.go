package conformance

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testDefinitionSnapshot covers run.started.definition_ref — the view-only snapshot of a run's
// canonical definition (recorded so a reader can render a past run against the structure it actually
// executed against, even after the file is edited). Two properties are pinned:
//  1. a run records a resolvable snapshot that reconstructs to the exact definition that ran
//     (same content digest);
//  2. the snapshot NEVER lets a resume bypass §8 pinning — drift against the live file is still a
//     hard error even with a snapshot present.
func testDefinitionSnapshot(t *testing.T, factory BackendFactory) {
	t.Helper()

	t.Run("run_records_resolvable_snapshot", func(t *testing.T) {
		wrapped := preProgramFake(t, factory, []execProgram{
			{cmd: "./step1.sh", res: container.ExecResult{ExitCode: 0}},
			{cmd: "./step2.sh", res: container.ExecResult{ExitCode: 0}},
		})
		h := newHarness(t, wrapped, tinySeqWorkflow)
		oc, err := h.runWorkflow(t)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if oc != engine.OutcomeOK {
			t.Fatalf("outcome = %q, want ok", oc)
		}

		rsd := mustRunStartedData(t, h)
		if rsd.DefinitionRef == "" {
			t.Fatal("run.started.definition_ref is empty; expected a snapshot ref")
		}
		snap, err := engine.LoadRunStartedDefinitionSnapshot(h.blobs, rsd.DefinitionRef)
		if err != nil {
			t.Fatalf("load snapshot: %v", err)
		}
		// The reconstructed definition must be the one that ran: identical content digest.
		gotDigest, err := snap.ComputeDigest()
		if err != nil {
			t.Fatalf("snapshot ComputeDigest: %v", err)
		}
		if gotDigest != rsd.WorkflowDigest {
			t.Fatalf("reconstructed snapshot digest = %q, want recorded %q", gotDigest, rsd.WorkflowDigest)
		}
	})

	t.Run("snapshot_does_not_bypass_resume_pinning", func(t *testing.T) {
		wrapped := preProgramFake(t, factory, []execProgram{
			{cmd: "./step1.sh", res: container.ExecResult{ExitCode: 0}},
			{cmd: "./step2.sh", res: container.ExecResult{ExitCode: 0}},
		})
		h := newHarness(t, wrapped, tinySeqWorkflow)
		if _, err := h.runWorkflow(t); err != nil {
			t.Fatalf("first run: %v", err)
		}
		// The guard is only meaningful if a snapshot was actually recorded for this run.
		if mustRunStartedData(t, h).DefinitionRef == "" {
			t.Fatal("expected a definition_ref to be present so the guard is meaningful")
		}
		preCount := len(mustFoldEvents(t, h))

		if err := os.WriteFile(h.wfPath, []byte(tinySeqWorkflowMutated), 0o644); err != nil {
			t.Fatalf("WriteFile mutated: %v", err)
		}
		_, err := h.resumeWorkflow(t)
		if err == nil {
			t.Fatal("resume against mutated workflow (snapshot present): err = nil, want digest-mismatch")
		}
		var dme *digestMismatchError
		if !errors.As(err, &dme) {
			t.Errorf("err = %v, want *digestMismatchError — the snapshot must not bypass pinning", err)
		}
		post := mustFoldEvents(t, h)
		if len(post) != preCount {
			t.Errorf("log advanced past refusal: pre=%d post=%d events", preCount, len(post))
		}
		for _, e := range post {
			if e.Type == engine.EventRunResumed {
				t.Error("digest-mismatch refusal must NOT append run.resumed")
			}
		}
	})
}

// mustRunStartedData folds the run's log and returns the decoded run.started payload.
func mustRunStartedData(t *testing.T, h *harness) engine.RunStartedData {
	t.Helper()
	for _, e := range mustFoldEvents(t, h) {
		if e.Type == engine.EventRunStarted {
			var d engine.RunStartedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal run.started: %v", err)
			}
			return d
		}
	}
	t.Fatal("no run.started event")
	return engine.RunStartedData{}
}
