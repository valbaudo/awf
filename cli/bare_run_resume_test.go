package cli_test

// F4b: RESUME of a run with an uncommitted bare `run:` step must re-provision
// the per-step implicit host-workspace handle and continue — the SAME F4a
// mechanism (engine/interpreter.go, hostWorkspaceSpec / BareRunHandleKey)
// that fires on a fresh run, since cli/resume.go dispatches through the
// identical r.runAndFinish -> engine.Run path as cli/run.go (cli/execute.go).
// No CLI-side re-provisioning code exists (or is needed) for this — this test
// exists to VERIFY that, not to add it.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

func TestCLIBareRunResumeReprovisionsUncommittedHandle(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "test-bare-run-resume"

	wfPath := filepath.Join(t.TempDir(), "bare-resume.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: bare-resume
version: 1
graph:
  - id: step1
    run: "echo one"
  - id: bare
    run: "echo two"
`), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	// Hand-craft an in-flight log: run.started + node.completed(step1) only —
	// models a SIGKILL crash after step1 committed but before "bare" ever
	// dispatched (mirrors TestCLIResumeHappyPathSkipsCommittedSteps's
	// hand-crafted-log rationale: cli/run.go always writes run.finished on
	// engine.Run return, so an in-flight log can't be produced via the CLI
	// alone). Backend is explicitly "fake" (NOT left blank — readBackendKindFromLog
	// defaults an empty Backend field to docker, which would trip F4b's own
	// AWF1065 guard on resume and defeat this test).
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	ld, err := loader.Load(wfPath)
	if err != nil {
		t.Fatalf("Load fixture: %v", err)
	}
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		t.Fatalf("fixture invalid: %v", diags)
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	log, err := state.OpenLogExclusive(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLogExclusive: %v", err)
	}
	runStartedData, err := json.Marshal(engine.RunStartedData{
		RunID: runID, WorkflowDigest: digest, Backend: engine.BackendFake,
	})
	if err != nil {
		t.Fatalf("Marshal run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: runStartedData}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	stdoutRef, err := blobs.Put([]byte("one\n"))
	if err != nil {
		t.Fatalf("Put stdout: %v", err)
	}
	exit0 := 0
	completedData, err := json.Marshal(engine.NodeCompletedData{
		Outcome: string(engine.OutcomeOK), ExitCode: &exit0, StdoutRef: stdoutRef,
	})
	if err != nil {
		t.Fatalf("Marshal node.completed: %v", err)
	}
	if err := log.Append(state.Event{
		Type: engine.EventNodeCompleted, Path: "step1", Data: completedData,
	}); err != nil {
		t.Fatalf("Append node.completed: %v", err)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Resume; program ONLY "echo two" (the uncommitted "bare" step). If
	// resume tried to re-run "step1" (a committed-step regression) the fake
	// would error with "no programmed result for cmd.Run=\"echo one\"".
	fake := container.NewFake()
	fake.ProgramExec("echo two", container.ExecResult{ExitCode: 0}, nil)
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"resume", "--state-dir", stateDir, runID, wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}

	// The per-step implicit handle was actually (re-)provisioned during
	// resume, keyed by hostWorkspaceSpec's deterministic "_run.<nodePath>"
	// naming — not silently skipped/reused from the (non-existent, this is a
	// brand-new *container.Fake) prior run.
	var sawBareCreate bool
	for _, spec := range fake.CreateSpecs {
		if spec.Name == "_run.bare" {
			sawBareCreate = true
			if spec.Image != "" {
				t.Errorf("CreateSpecs[_run.bare].Image = %q, want empty (host workspace, no image)", spec.Image)
			}
		}
	}
	if !sawBareCreate {
		t.Fatalf("no Create call for \"_run.bare\"; CreateSpecs = %+v (per-step handle not re-provisioned on resume)", fake.CreateSpecs)
	}

	// The resumed run actually completed "bare", end to end.
	logReopen, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog post-resume: %v", err)
	}
	defer func() { _ = logReopen.Close() }()
	events, err := logReopen.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var sawBareCompleted, sawFinishedOK bool
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted && e.Path == "bare" {
			sawBareCompleted = true
		}
		if e.Type == engine.EventRunFinished {
			var d engine.RunFinishedData
			if jsonErr := json.Unmarshal(e.Data, &d); jsonErr == nil && d.Outcome == "ok" {
				sawFinishedOK = true
			}
		}
	}
	if !sawBareCompleted {
		t.Errorf("no node.completed for \"bare\" after resume; events: %+v", events)
	}
	if !sawFinishedOK {
		t.Errorf("no run.finished{ok} after resume; events: %+v", events)
	}
}
