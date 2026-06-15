package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/valbaudo/awf/agent"
	agentfake "github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

// firstRunSeq runs the seq.yaml fixture from scratch via `awf run`, returning
// the state directory + the minted run id. Resume tests call this to produce
// a baseline log they then interrogate.
func firstRunSeq(t *testing.T) (stateDir, runID string) {
	t.Helper()
	fake := container.NewFake()
	fake.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`),
	}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)

	stateDir = t.TempDir()
	runID = "test-resume-baseline"
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{IDs: []string{runID}}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--state-dir", stateDir, "--run-id", runID,
		"testdata/phase2/seq.yaml",
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("first run rc = %d, want %d; stderr: %s", rc, cli.ExitOK, stderr.String())
	}
	return stateDir, runID
}

func TestCLIResumeRefusesUnknownRunID(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"resume", "--state-dir", stateDir, "does-not-exist", "testdata/phase2/seq.yaml",
	}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (unknown run id)", rc)
	}
	if !strings.Contains(stderr.String(), "does-not-exist") {
		t.Errorf("stderr missing run id: %q", stderr.String())
	}
}

func TestCLIResumeRefusesTerminalRunFinished(t *testing.T) {
	t.Parallel()
	stateDir, runID := firstRunSeq(t)
	// The baseline first run ran to completion → log has run.finished.
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq.yaml",
	}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (run already finished)", rc)
	}
	if !strings.Contains(stderr.String(), "Nothing to resume") {
		t.Errorf("stderr missing 'Nothing to resume': %q", stderr.String())
	}
}

func TestCLIResumeAdmitsNodeFailedCrashWindow(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "test-resume-node-failed"

	// Hand-craft a log with run.started + node.failed{permanent_failure} and NO
	// run.finished, modelling a SIGKILL crash after node.failed but before
	// run.finished. The crash-window node.failed (no run.finished rollup) is now
	// ADMITTED SILENTLY (label "", spec D2) — there is no terminal-resume barrier.
	// The hand-crafted log uses an all-zeros digest that cannot match seq.yaml,
	// so after the silent admit the resume reaches the digest-mismatch hard error
	// (pinning is NOT relaxed). That hard error is the proof the guard admitted.
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := state.OpenBlobs(filepath.Join(stateDir, "blobs")); err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	log, err := state.OpenLogExclusive(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLogExclusive: %v", err)
	}
	// Arbitrary digest — node.failed refusal fires BEFORE the digest
	// check in the resume flow, so the value never gets compared.
	runStartedData, err := json.Marshal(engine.RunStartedData{
		RunID:          runID,
		WorkflowDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err != nil {
		t.Fatalf("Marshal run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: runStartedData}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	failedData, err := json.Marshal(engine.NodeFailedData{
		Outcome: string(engine.OutcomePermanentFailure),
		Error:   "exit code 78 (declared non-retryable)",
	})
	if err != nil {
		t.Fatalf("Marshal node.failed: %v", err)
	}
	if err := log.Append(state.Event{
		Type: engine.EventNodeFailed, Path: "step1", Data: failedData,
	}); err != nil {
		t.Fatalf("Append node.failed: %v", err)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq.yaml",
	}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (digest mismatch after silent crash-window admit)", rc)
	}
	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "digest mismatch") {
		t.Errorf("guard did not admit a crash-window node.failed run: %q", stderrStr)
	}
	if strings.Contains(stderrStr, "Re-run with --force") || strings.Contains(stderrStr, "terminated on a failed step") {
		t.Errorf("guard wrongly refused a crash-window run: %q", stderrStr)
	}
}

func TestCLIResumeHappyPathSkipsCommittedSteps(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "test-resume-happy"

	// Hand-craft an in-flight log: run.started + node.completed(touch_marker).
	// The CLI's slice 2.5 cli/run.go always writes run.finished on engine.Run
	// return — so we cannot produce an "in-flight" log via the CLI alone in a
	// unit test (subprocess fork-and-kill is YAGNI per spec §H). The
	// hand-crafted log models a real SIGKILL where defers don't run.
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	ld, err := loader.Load("testdata/phase2/seq.yaml")
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
	runStartedData, _ := json.Marshal(engine.RunStartedData{
		RunID: runID, WorkflowDigest: digest,
	})
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: runStartedData}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	stdoutRef, err := blobs.Put([]byte("created marker\n"))
	if err != nil {
		t.Fatalf("Put stdout: %v", err)
	}
	exit0 := 0
	completedData, _ := json.Marshal(engine.NodeCompletedData{
		Outcome: string(engine.OutcomeOK), ExitCode: &exit0, StdoutRef: stdoutRef,
	})
	if err := log.Append(state.Event{
		Type: engine.EventNodeCompleted, Path: "touch_marker", Data: completedData,
	}); err != nil {
		t.Fatalf("Append node.completed: %v", err)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Resume against the same fixture; program ONLY steps 2 and 3.
	fake := container.NewFake()
	fake.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`),
	}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq.yaml",
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	// touch_marker MUST NOT have been dispatched.
	if len(fake.Calls) != 2 {
		t.Fatalf("fake.Calls len = %d, want 2 (touch_marker must NOT re-execute)", len(fake.Calls))
	}
	if fake.Calls[0].Run != "echo step2" {
		t.Errorf("fake.Calls[0].Run = %q, want %q", fake.Calls[0].Run, "echo step2")
	}
	// Log now contains run.resumed{epoch:2} + 2 new node.completed +
	// run.finished{ok}.
	logReopen, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog post-resume: %v", err)
	}
	defer func() { _ = logReopen.Close() }()
	events, _ := logReopen.Fold()
	var resumed, completed, finished int
	var resumedEpoch uint32
	var finishedOutcome string
	for _, e := range events {
		switch e.Type {
		case engine.EventRunResumed:
			resumed++
			var d engine.RunResumedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal run.resumed: %v", err)
			}
			resumedEpoch = d.Epoch
		case engine.EventNodeCompleted:
			completed++
		case engine.EventRunFinished:
			finished++
			var d engine.RunFinishedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal run.finished: %v", err)
			}
			finishedOutcome = d.Outcome
		}
	}
	if resumed != 1 || resumedEpoch != 2 {
		t.Errorf("run.resumed: count=%d epoch=%d, want 1/2", resumed, resumedEpoch)
	}
	if completed != 3 {
		t.Errorf("node.completed total = %d, want 3 (1 pre-resume + 2 post-resume)", completed)
	}
	if finished != 1 || finishedOutcome != "ok" {
		t.Errorf("run.finished = %d events outcome=%q, want 1/ok", finished, finishedOutcome)
	}
}

func TestCLIResumeAdmitsCancelledRun(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "test-resume-cancelled"
	// Hand-craft a log with run.started + run.cancelled. A cancelled run is now
	// admitted; the hand-crafted digest "abc" cannot match seq.yaml, so the
	// admitted resume reaches the digest-mismatch hard error (the proof it admitted).
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := state.OpenBlobs(filepath.Join(stateDir, "blobs")); err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	log, err := state.OpenLogExclusive(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLogExclusive: %v", err)
	}
	startedData, _ := json.Marshal(engine.RunStartedData{
		RunID: runID, WorkflowDigest: "abc",
	})
	cancelledData, _ := json.Marshal(engine.RunCancelledData{Reason: "test"})
	for _, e := range []state.Event{
		{Type: engine.EventRunStarted, Data: startedData},
		{Type: engine.EventRunCancelled, Data: cancelledData},
	} {
		if err := log.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	_ = log.Sync()
	_ = log.Close()

	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq.yaml",
	}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (digest mismatch after admit)", rc)
	}
	if !strings.Contains(stderr.String(), "digest mismatch") {
		t.Errorf("guard did not admit a cancelled run: %q", stderr.String())
	}
}

func TestCLIResumeRejectsBackendFlag(t *testing.T) {
	t.Parallel()
	// Resume DOES NOT accept --backend (per Phase 4 design § F). The
	// flag.NewFlagSet for resume doesn't declare it; flag.Parse rejects
	// the unknown flag with "flag provided but not defined: -backend".
	//
	// Stronger pin than rc-only: also asserts the stderr wording. This
	// matches slice 3.5's TestCLIPauseRejectsBeforeFlag pattern at
	// cli/pause_test.go:58, which asserts a specific deferral message.
	// Without the substring check, a future refactor that adds --backend
	// to resume would pass the test (the run-not-found path also returns
	// ExitUsage) — true regression undetected. "not defined" is Go's
	// standard flag-package wording for unknown flags; if stdlib ever
	// changes it, this test will need updating, which is the right cost
	// to pay for catching the regression class this pin exists for.
	stateDir := t.TempDir()
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{"resume", "--state-dir", stateDir, "--backend", "fake", "some-run-id", "testdata/phase2/seq.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitUsage {
		t.Errorf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not defined") {
		t.Errorf("stderr missing Go flag-package rejection wording 'not defined'; got %q. If a future refactor added --backend to resume, the rc-only check above would still pass (run-not-found also exits ExitUsage) — this substring is the actual regression pin.", stderr.String())
	}
}

func TestCLIResumeRejectsNativeLogWithDockerGuidance(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "native-open"
	runDir := filepath.Join(stateDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	log, err := state.OpenLog(filepath.Join(runDir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	startedData, err := json.Marshal(engine.RunStartedData{
		RunID:   runID,
		Backend: engine.BackendNative,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: startedData}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	runner := &cli.Runner{}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not resumable") {
		t.Errorf("stderr missing native-resume limitation: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--backend docker") {
		t.Errorf("stderr missing docker guidance: %s", stderr.String())
	}
}

func TestCLIResumeDigestMismatchHardError(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "test-resume-digest-mismatch"

	ld, _ := loader.Load("testdata/phase2/seq.yaml")
	digest, _ := ld.ComputeDigest()
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := state.OpenBlobs(filepath.Join(stateDir, "blobs")); err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	log, _ := state.OpenLogExclusive(logPath, clock.System{})
	runStartedData, _ := json.Marshal(engine.RunStartedData{RunID: runID, WorkflowDigest: digest})
	_ = log.Append(state.Event{Type: engine.EventRunStarted, Data: runStartedData})
	_ = log.Sync()
	_ = log.Close()

	src, _ := os.ReadFile("testdata/phase2/seq.yaml")
	mutated := strings.Replace(string(src),
		"run: \"touch /tmp/awf-seq-marker\"",
		"run: \"touch /tmp/awf-seq-marker-MUTATED\"", 1)
	if mutated == string(src) {
		t.Fatal("mutation no-op")
	}
	mutatedPath := filepath.Join(t.TempDir(), "seq-mutated.yaml")
	if err := os.WriteFile(mutatedPath, []byte(mutated), 0o644); err != nil {
		t.Fatalf("WriteFile mutated: %v", err)
	}

	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"resume", "--state-dir", stateDir, runID, mutatedPath,
	}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (digest mismatch)", rc)
	}
	if !strings.Contains(stderr.String(), "digest mismatch") {
		t.Errorf("stderr missing 'digest mismatch': %q", stderr.String())
	}
	logReopen, _ := state.OpenLog(logPath, clock.System{})
	defer func() { _ = logReopen.Close() }()
	events, _ := logReopen.Fold()
	for _, e := range events {
		if e.Type == engine.EventRunResumed {
			t.Errorf("digest-mismatch refusal must NOT append run.resumed; events: %+v", events)
		}
	}
}

func TestCLIResumeAssetDigestMismatchBeforeMissingSnapshotBlob(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "test-resume-asset-digest-mismatch"
	dir := t.TempDir()
	assetPath := filepath.Join(dir, "asset.txt")
	if err := os.WriteFile(assetPath, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "workflow.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: asset-drift
version: 1
assets:
  input: asset.txt
containers: {}
graph: []
`), 0o644); err != nil {
		t.Fatal(err)
	}

	ld, err := loader.Load(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := state.OpenBlobs(filepath.Join(stateDir, "blobs")); err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	log, err := state.OpenLogExclusive(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLogExclusive: %v", err)
	}
	missingRef := "awf-d1:sha256:" + strings.Repeat("a", 64)
	runStartedData, err := json.Marshal(engine.RunStartedData{
		RunID:          runID,
		WorkflowDigest: digest,
		Backend:        engine.BackendFake,
		Assets: map[string]engine.RunStartedAsset{
			"input": {
				DeclaredPath: "asset.txt",
				Files: []engine.RunStartedAssetFile{{
					Path: ".", Ref: missingRef, Size: int64(len("original")),
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Marshal run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: runStartedData}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := os.WriteFile(assetPath, []byte("mutated"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"resume", "--state-dir", stateDir, runID, wfPath,
	}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Fatalf("rc = ExitOK, want digest mismatch; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "digest mismatch") {
		t.Fatalf("stderr missing digest mismatch: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), missingRef) || strings.Contains(stderr.String(), "not exist") {
		t.Fatalf("resume reported missing asset blob before digest drift: %s", stderr.String())
	}
	logReopen, _ := state.OpenLog(logPath, clock.System{})
	defer func() { _ = logReopen.Close() }()
	events, _ := logReopen.Fold()
	for _, e := range events {
		if e.Type == engine.EventRunResumed {
			t.Errorf("asset digest-mismatch refusal must NOT append run.resumed; events: %+v", events)
		}
	}
}

func TestErrRuntimeDrift_Format(t *testing.T) {
	err := &cli.ErrRuntimeDrift{
		Ref:       "anthropic/claude-code",
		Container: "lab",
		Recorded:  "2.1.118",
		Current:   "2.1.150",
	}
	want := `cli: agent runtime drift for "anthropic/claude-code" in container "lab": recorded "2.1.118", now "2.1.150" — cannot resume (spec §8 pinning is a hard error)`
	if err.Error() != want {
		t.Errorf("Error() =\n  %q\nwant:\n  %q", err.Error(), want)
	}
}

func TestErrRuntimeDrift_AsTarget(t *testing.T) {
	inner := &cli.ErrRuntimeDrift{Ref: "x", Container: "y", Recorded: "1", Current: "2"}
	wrapped := errors.Join(errors.New("ctx"), inner)
	var target *cli.ErrRuntimeDrift
	if !errors.As(wrapped, &target) {
		t.Fatalf("errors.As did not unwrap to *cli.ErrRuntimeDrift")
	}
	if target.Ref != "x" {
		t.Errorf("Ref = %q, want %q", target.Ref, "x")
	}
}

func TestCLIResume_RuntimeDriftHardError(t *testing.T) {
	t.Skip("End-to-end CLI test deferred until slice 5.2 (AgentStep dispatcher). Slice 5.1's coverage is the unit-level drift check in cli/runtimes_test.go.")
}

func TestResumeRefusesWhenRunLockHeld(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "lk-resume"
	runDir := filepath.Join(stateDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Minimal openable log: just run.started.
	lg, err := state.OpenLog(filepath.Join(runDir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	rsd, _ := json.Marshal(engine.RunStartedData{RunID: runID})
	if err := lg.Append(state.Event{Type: engine.EventRunStarted, Data: rsd}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	_ = lg.Sync()
	_ = lg.Close()

	// Hold the run lock as if another process were actively driving the run.
	lf, err := os.OpenFile(filepath.Join(runDir, "run.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open run.lock: %v", err)
	}
	defer func() { _ = lf.Close() }()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{IDs: []string{runID}}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("resume of a locked (live) run: rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already active") {
		t.Errorf("expected an 'already active' refusal, got: %s", stderr.String())
	}
}

// TestCLIResume_PopulatesResolverFromDefaultAllowlist verifies that `awf
// resume` (Task 20) builds a production *agent.Registry from the SAME
// default env-var allowlist used by `awf run` when r.Resolver is nil.
// Uses a hand-crafted in-flight log (run.started + one node.completed,
// no run.finished) so the resume call actually reaches the backend
// TestCLIResume_WorkflowEnv_FoldsIntoDigestAndResumes guards the gap-1 resume
// path: a workflow declaring a top-level env: must (a) fold those NAMES into the
// definition digest identically on the run side (here, the hand-crafted log's
// digest) and the resume side, so resume does NOT trip the digest-mismatch hard
// error, and (b) still reach the registry build — which slice-5.3 moved to AFTER
// the load+digest checks so ld.Workflow.Env can extend the allowlist. If env: were
// folded asymmetrically, or the registry build were misordered, this fails.
func TestCLIResume_WorkflowEnv_FoldsIntoDigestAndResumes(t *testing.T) {
	t.Setenv("CUSTOM_AGENT_TOKEN", "custom-secret")
	stateDir := t.TempDir()
	runID := "test-resume-workflow-env"

	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	tmpDir := t.TempDir()
	wfPath := filepath.Join(tmpDir, "resume-env.awf.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: resume-env
version: 1
env: [CUSTOM_AGENT_TOKEN]
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: touch_marker
    container: lab
    run: "true"
  - id: ask
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: resume please
      bare: false
`), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	// Digest is computed from the env:-bearing fixture — the env names fold in.
	ld, err := loader.Load(wfPath)
	if err != nil {
		t.Fatalf("Load fixture: %v", err)
	}
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		t.Fatalf("fixture invalid: %v", diags)
	}
	if len(ld.Workflow.Env) == 0 {
		t.Fatal("fixture lost its env: declaration on load")
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
	runStartedData, _ := json.Marshal(engine.RunStartedData{
		RunID: runID, WorkflowDigest: digest, Backend: "fake",
		Runtimes: []engine.ResolvedRuntime{
			{Ref: "anthropic/claude-code", Version: "2.1.0", Container: "lab"},
		},
	})
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: runStartedData}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	stdoutRef, err := blobs.Put([]byte("created marker\n"))
	if err != nil {
		t.Fatalf("Put stdout: %v", err)
	}
	exit0 := 0
	completedData, _ := json.Marshal(engine.NodeCompletedData{
		Outcome: string(engine.OutcomeOK), ExitCode: &exit0, StdoutRef: stdoutRef,
	})
	if err := log.Append(state.Event{Type: engine.EventNodeCompleted, Path: "touch_marker", Data: completedData}); err != nil {
		t.Fatalf("Append node.completed: %v", err)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fake := container.NewFake()
	programClaudeSuccess(fake)
	r := &cli.Runner{Backend: fake, IDGen: &clock.Fake{IDs: []string{"unused"}}}
	var stdout, stderr bytes.Buffer
	exit := r.Run([]string{"resume", "--state-dir", stateDir, runID, wfPath}, &stdout, &stderr)
	// The digest matched (env: folded symmetrically) so resume did NOT refuse;
	// the invocation-local registry build then forwards the workflow env name
	// into the resumed agent launch.
	if strings.Contains(stderr.String(), "digest mismatch") {
		t.Fatalf("resume refused on digest mismatch with env: present — env names folded asymmetrically: %s", stderr.String())
	}
	if exit != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", exit, cli.ExitOK, stderr.String())
	}
	var sawCustomEnv bool
	for _, call := range fake.Calls {
		if call.Run == "claude --version" {
			continue
		}
		if call.Env["CUSTOM_AGENT_TOKEN"] == "custom-secret" {
			sawCustomEnv = true
		}
	}
	if !sawCustomEnv {
		t.Fatalf("resumed agent launch did not receive workflow env CUSTOM_AGENT_TOKEN; calls=%+v", fake.Calls)
	}
	if r.Resolver != nil {
		t.Error("Resolver was cached after resume of an env:-bearing workflow; want production registry scoped to the invocation")
	}
}

// resolution + Resolver-population path instead of short-circuiting on
// a terminal-event refusal.
func TestCLIResume_PopulatesResolverFromDefaultAllowlist(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-fixture")
	stateDir := t.TempDir()
	runID := "test-resume-populates-resolver"

	// Hand-craft an in-flight log: run.started (with backend=fake) +
	// node.completed for the only step.
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	ld, err := loader.Load("testdata/phase2/seq.yaml")
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
	runStartedData, _ := json.Marshal(engine.RunStartedData{
		RunID:          runID,
		WorkflowDigest: digest,
		Backend:        "fake",
	})
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: runStartedData}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	stdoutRef, err := blobs.Put([]byte("created marker\n"))
	if err != nil {
		t.Fatalf("Put stdout: %v", err)
	}
	exit0 := 0
	completedData, _ := json.Marshal(engine.NodeCompletedData{
		Outcome: string(engine.OutcomeOK), ExitCode: &exit0, StdoutRef: stdoutRef,
	})
	if err := log.Append(state.Event{
		Type: engine.EventNodeCompleted, Path: "touch_marker", Data: completedData,
	}); err != nil {
		t.Fatalf("Append node.completed: %v", err)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Resume against the in-flight log. Don't program the fake — the resume
	// may fail later (e.g. on remaining-step dispatch), but Resolver must
	// already be populated by the time we look.
	fake := container.NewFake()
	fake.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`),
	}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	r := &cli.Runner{
		Backend: fake,
		IDGen:   &clock.Fake{IDs: []string{"unused"}},
	}
	var stdout, stderr bytes.Buffer
	_ = r.Run([]string{"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if r.Resolver != nil {
		t.Error("Resolver was cached after resume; want production registry scoped to the invocation")
	}
}

// buildInFlightLogForWF writes a minimal in-flight log (run.started only,
// no run.finished) for the workflow file at wfPath, using the given runID
// and the supplied runtimes slice (stored in RunStartedData.Runtimes for the
// resume-side drift check). Returns the stateDir so callers can pass it to
// awf resume.
func buildInFlightLogForWF(t *testing.T, wfPath, runID string, runtimes []engine.ResolvedRuntime) string {
	t.Helper()
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := state.OpenBlobs(filepath.Join(stateDir, "blobs")); err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	ld, err := loader.Load(wfPath)
	if err != nil {
		t.Fatalf("loader.Load(%q): %v", wfPath, err)
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
	rsd, err := json.Marshal(engine.RunStartedData{
		RunID: runID, WorkflowDigest: digest, Backend: "fake",
		Runtimes: runtimes,
	})
	if err != nil {
		t.Fatalf("Marshal run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: rsd}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return stateDir
}

func TestResumeDigestDriftFromImportedWorkflowFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.awf.yaml")
	if err := os.WriteFile(childPath, []byte(`workflow: child
version: 1
containers: {}
graph: []
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(dir, "root.awf.yaml")
	if err := os.WriteFile(rootPath, []byte(`workflow: root
version: 1
imports:
  recon: child.awf.yaml
containers: {}
graph: []
`), 0o644); err != nil {
		t.Fatal(err)
	}
	runID := "test-import-digest-drift"
	stateDir := buildInFlightLogForWF(t, rootPath, runID, nil)

	if err := os.WriteFile(childPath, []byte(`workflow: child-mutated
version: 1
containers: {}
graph: []
`), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, rootPath}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "workflow digest mismatch") {
		t.Fatalf("stderr = %q, want digest mismatch", stderr.String())
	}
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	lg, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = lg.Close() }()
	events, err := lg.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	for _, e := range events {
		if e.Type == engine.EventRunResumed {
			t.Fatal("run.resumed found after imported digest drift rejection")
		}
	}
}

// TestResume_ContinuesAgainstNonThreadedAdapter_FailsFast is the T8 end-to-end
// for `awf resume`: a continues: step whose adapter is NOT Threaded must be
// rejected before run.resumed is appended, with ExitUsage and the
// ErrThreadedRequired message.
func TestResume_ContinuesAgainstNonThreadedAdapter_FailsFast(t *testing.T) {
	t.Parallel()
	fk := agentfake.New("anthropic/claude-code") // default Caps: Threaded false
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	tmp := t.TempDir()
	// Write a continues: workflow against the non-threaded adapter.
	wfPath := filepath.Join(tmp, "continues-wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: continues-resume-test
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: draft
    uses: anthropic/claude-code
    container: lab
  - id: refine
    uses: anthropic/claude-code
    container: lab
    continues: draft
`), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	runID := "test-threaded-guard-resume"
	// Pre-populate Runtimes in run.started to match what resolveRuntimes would
	// return for the two claude-code steps (one deduped (uses,container) pair,
	// default fake version "fake-v1").
	runtimes := []engine.ResolvedRuntime{
		{Ref: "anthropic/claude-code", Version: "fake-v1", Container: "lab"},
	}
	stateDir := buildInFlightLogForWF(t, wfPath, runID, runtimes)

	r := &cli.Runner{
		Backend:  container.NewFake(),
		IDGen:    &clock.Fake{IDs: []string{runID}},
		Resolver: &reg,
	}
	var stdout, stderr bytes.Buffer
	rc := r.Run([]string{"resume", "--state-dir", stateDir, runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage (%d); stderr: %s", rc, cli.ExitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not support engine-threaded conversations") {
		t.Fatalf("stderr = %q, want ErrThreadedRequired message", stderr.String())
	}
	// Guard fires BEFORE run.resumed is appended: fold the log and confirm no
	// run.resumed event.
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	lg, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = lg.Close() }()
	events, err := lg.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	for _, e := range events {
		if e.Type == engine.EventRunResumed {
			t.Errorf("run.resumed found in log after guard rejection; guard must fire before appending it")
		}
	}
}

// TestResume_ContinuesAgainstThreadedAdapter_OK confirms that a continues:
// step whose adapter IS Threaded (awf/llm) passes the guard on resume.
func TestResume_ContinuesAgainstThreadedAdapter_OK(t *testing.T) {
	t.Parallel()
	fk := agentfake.New("awf/llm").WithCaps(agent.Caps{Containerless: true, Threaded: true})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	tmp := t.TempDir()
	// Write a containerless continues: workflow against the Threaded adapter.
	wfPath := filepath.Join(tmp, "continues-threaded-wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: continues-threaded-resume-test
version: 1
graph:
  - id: draft
    uses: awf/llm
  - id: refine
    uses: awf/llm
    continues: draft
`), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	runID := "test-threaded-ok-resume"
	// Containerless adapter: one (uses, "") pair with default fake version.
	runtimes := []engine.ResolvedRuntime{
		{Ref: "awf/llm", Version: "fake-v1", Container: ""},
	}
	stateDir := buildInFlightLogForWF(t, wfPath, runID, runtimes)

	r := &cli.Runner{
		Backend:  container.NewFake(),
		IDGen:    &clock.Fake{IDs: []string{runID}},
		Resolver: &reg,
	}
	var stdout, stderr bytes.Buffer
	rc := r.Run([]string{"resume", "--state-dir", stateDir, runID, wfPath}, &stdout, &stderr)
	// Guard passes; the run may fail at dispatch (fake not programmed), but
	// NOT with the ErrThreadedRequired message.
	if rc == cli.ExitUsage && strings.Contains(stderr.String(), "does not support engine-threaded conversations") {
		t.Fatalf("Threaded adapter incorrectly rejected by guard on resume; stderr: %s", stderr.String())
	}
}

func TestResumePersistentAdapterRequiresResumePreflighter(t *testing.T) {
	t.Parallel()

	fk := agentfake.New("live/no-preflight").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dir := t.TempDir()
	wfPath := filepath.Join(dir, "live-no-preflight.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: live-no-preflight
version: 1
graph:
  - id: live
    uses: live/no-preflight
`), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	runID := "test-live-preflight-required"
	stateDir := buildInFlightLogForWF(t, wfPath, runID, []engine.ResolvedRuntime{
		{Ref: "live/no-preflight", Version: "fake-v1"},
	})

	r := &cli.Runner{Backend: container.NewFake(), Resolver: &reg, IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := r.Run([]string{"resume", "--state-dir", stateDir, runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ResumePreflighter") {
		t.Fatalf("stderr = %q, want ResumePreflighter requirement", stderr.String())
	}
	assertNoRunResumed(t, stateDir, runID)
}

func TestResumeLiveReplayRequiredBeforeRunResumed(t *testing.T) {
	t.Parallel()

	adapter := &resumePreflightAdapter{
		Fake: agentfake.New("live/replay").WithCaps(agent.Caps{
			NativeSchema:      true,
			Containerless:     true,
			PersistentSession: true,
		}),
		err: agent.ErrLiveReplayRequired,
	}
	var reg agent.Registry
	if err := reg.Register(adapter); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dir := t.TempDir()
	wfPath := filepath.Join(dir, "live-replay.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: live-replay
version: 1
graph:
  - id: live
    uses: live/replay
    with:
      session: awf-live
`), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	runID := "test-live-replay-required"
	stateDir := buildInFlightLogForWF(t, wfPath, runID, []engine.ResolvedRuntime{
		{Ref: "live/replay", Version: "fake-v1"},
	})

	r := &cli.Runner{Backend: container.NewFake(), Resolver: &reg, IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := r.Run([]string{"resume", "--state-dir", stateDir, runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "live replay required") {
		t.Fatalf("stderr = %q, want replay-required message", stderr.String())
	}
	assertNoRunResumed(t, stateDir, runID)
	if len(adapter.reqs) != 1 {
		t.Fatalf("preflight calls = %d, want 1", len(adapter.reqs))
	}
	if adapter.reqs[0].NodePath != "live" || adapter.reqs[0].RunID != runID {
		t.Fatalf("preflight request = %+v, want node/run context", adapter.reqs[0])
	}
}

type resumePreflightAdapter struct {
	*agentfake.Fake
	err  error
	reqs []agent.LiveResumePreflightRequest
}

func (a *resumePreflightAdapter) PreflightResume(_ context.Context, req agent.LiveResumePreflightRequest) error {
	a.reqs = append(a.reqs, req)
	return a.err
}

func assertNoRunResumed(t *testing.T, stateDir, runID string) {
	t.Helper()
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	lg, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = lg.Close() }()
	events, err := lg.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	for _, e := range events {
		if e.Type == engine.EventRunResumed {
			t.Fatalf("run.resumed found after preflight rejection; preflight must run before journal mutation")
		}
	}
}

// buildTwoStepOKRun produces a completed run on disk with two top-level code
// steps (a → b) in a single container with an all-zeros pinned image. Returns
// stateDir, runID, and wfPath so callers can resume against the SAME workflow
// file (identical digest).
func buildTwoStepOKRun(t *testing.T) (stateDir, runID, wfPath string) {
	t.Helper()
	tmp := t.TempDir()
	wfPath = filepath.Join(tmp, "two-step-wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: two-step-ok
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: a
    container: lab
    run: "./a.sh"
  - id: b
    container: lab
    run: "./b.sh"
`), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	runID = "test-twostep-ok"
	stateDir = t.TempDir()
	fake := container.NewFake()
	fake.ProgramExec("./a.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	fake.ProgramExec("./b.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{IDs: []string{runID}}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "--run-id", runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("buildTwoStepOKRun: expected ExitOK, got %d; stderr: %s", rc, stderr.String())
	}
	return stateDir, runID, wfPath
}

func TestCLIResumeFrom_ReRunsFromStep(t *testing.T) {
	t.Parallel()
	stateDir, runID, wfPath := buildTwoStepOKRun(t) // a(./a.sh)->b(./b.sh), both committed ok
	// Reprogram so re-running is observable: step a's command must execute again.
	fake := container.NewFake()
	fake.ProgramExec("./a.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	fake.ProgramExec("./b.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, "--from", "a", runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	// a + b both re-ran (a re-runs because --from a; b because it is after a).
	var ranA, ranB bool
	for _, c := range fake.Calls {
		if c.Run == "./a.sh" {
			ranA = true
		}
		if c.Run == "./b.sh" {
			ranB = true
		}
	}
	if !ranA || !ranB {
		t.Fatalf("expected a and b to re-run; ranA=%v ranB=%v", ranA, ranB)
	}
	if !strings.Contains(stderr.String(), "re-run") {
		t.Fatalf("expected the re-run set disclosure on stderr; got %q", stderr.String())
	}
}

func TestCLIResumeFrom_BypassesDigestPin(t *testing.T) {
	t.Parallel()
	stateDir, runID, wfPath := buildTwoStepOKRun(t)
	// Mutate the wf so its digest differs but it still validates (change a container digest).
	orig, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatalf("read wf: %v", err)
	}
	mutated := strings.Replace(string(orig), "sha256:0000000000000000000000000000000000000000000000000000000000000000", "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 1)
	if mutated == string(orig) {
		t.Fatal("mutation no-op; adjust the digest literal to match the fixture")
	}
	if err := os.WriteFile(wfPath, []byte(mutated), 0o600); err != nil {
		t.Fatalf("write mutated wf: %v", err)
	}
	fake := container.NewFake()
	fake.ProgramExec("./a.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	fake.ProgramExec("./b.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{}`)}, nil)
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, "--from", "a", runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK (--from bypasses the digest pin); stderr: %s", rc, stderr.String())
	}
}

// buildResumeLog writes a hand-crafted log under a fresh stateDir for runID:
// run.started (digest of wfPath) followed by the given terminal events. Mirrors
// the inline fixture in TestCLIResumeDigestMismatchHardError (Backend field
// omitted — resolveBackend tolerates it and reaches the digest check).
func buildResumeLog(t *testing.T, wfPath, runID string, terminal ...state.Event) string {
	t.Helper()
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := state.OpenBlobs(filepath.Join(stateDir, "blobs")); err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	ld, err := loader.Load(wfPath)
	if err != nil {
		t.Fatalf("loader.Load(%q): %v", wfPath, err)
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
	rsd, err := json.Marshal(engine.RunStartedData{RunID: runID, WorkflowDigest: digest})
	if err != nil {
		t.Fatalf("Marshal run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: rsd}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	for _, e := range terminal {
		if err := log.Append(e); err != nil {
			t.Fatalf("Append %s: %v", e.Type, err)
		}
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return stateDir
}

func nodeFailedEvent(t *testing.T, path string, oc engine.Outcome) state.Event {
	t.Helper()
	d, err := json.Marshal(engine.NodeFailedData{Outcome: string(oc)})
	if err != nil {
		t.Fatalf("marshal node.failed: %v", err)
	}
	return state.Event{Type: engine.EventNodeFailed, Path: path, Data: d}
}

func runFinishedEvent(t *testing.T, oc engine.Outcome) state.Event {
	t.Helper()
	d, err := json.Marshal(engine.RunFinishedData{Outcome: string(oc)})
	if err != nil {
		t.Fatalf("marshal run.finished: %v", err)
	}
	return state.Event{Type: engine.EventRunFinished, Data: d}
}

// mutatedSeqPath returns a temp copy of seq.yaml with one run: line changed, so
// its digest differs from the original. Resuming against it after admission
// triggers the "digest mismatch" hard error — our proof that the guard admitted.
func mutatedSeqPath(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("testdata/phase2/seq.yaml")
	if err != nil {
		t.Fatalf("read seq.yaml: %v", err)
	}
	mutated := strings.Replace(string(src),
		"run: \"touch /tmp/awf-seq-marker\"",
		"run: \"touch /tmp/awf-seq-marker-MUTATED\"", 1)
	if mutated == string(src) {
		t.Fatal("mutation no-op")
	}
	p := filepath.Join(t.TempDir(), "seq-mutated.yaml")
	if err := os.WriteFile(p, []byte(mutated), 0o644); err != nil {
		t.Fatalf("WriteFile mutated: %v", err)
	}
	return p
}

// ADMIT: run.finished{retryable_failure} co-existing with a nested permanent
// node.failed (tolerated map item). The OLD guard refused; the new guard admits
// (run.finished is sole authority) → reaches the digest check. THE regression test.
func TestCLIResumeAdmitsRetryableDespiteNestedPermanent(t *testing.T) {
	t.Parallel()
	runID := "test-resume-admit-compound"
	stateDir := buildResumeLog(t, "testdata/phase2/seq.yaml", runID,
		nodeFailedEvent(t, "map[0].item-2", engine.OutcomePermanentFailure),
		runFinishedEvent(t, engine.OutcomeRetryableFailure),
	)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, mutatedSeqPath(t)}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (digest mismatch after admit)", rc)
	}
	got := stderr.String()
	if !strings.Contains(got, "digest mismatch") {
		t.Errorf("guard did not admit (no digest-mismatch reached): %q", got)
	}
	if strings.Contains(got, "already finished") || strings.Contains(got, "Not resumable") {
		t.Errorf("guard wrongly refused a retryable run: %q", got)
	}
}

// ADMIT: crash window — node.failed{retryable_failure}, no run.finished.
func TestCLIResumeAdmitsRetryableCrashWindow(t *testing.T) {
	t.Parallel()
	runID := "test-resume-admit-crash"
	stateDir := buildResumeLog(t, "testdata/phase2/seq.yaml", runID,
		nodeFailedEvent(t, "echo_step", engine.OutcomeRetryableFailure),
	)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, mutatedSeqPath(t)}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (digest mismatch after admit)", rc)
	}
	got := stderr.String()
	if !strings.Contains(got, "digest mismatch") {
		t.Errorf("guard did not admit a retryable crash-window run: %q", got)
	}
	if strings.Contains(got, "Re-run with --force") {
		t.Errorf("guard wrongly refused a retryable crash-window run: %q", got)
	}
}

// ADMIT: run.finished{permanent_failure}. Admission is proven by the digest
// mismatch reached after admit (mirrors TestCLIResumeAdmitsRetryableCrashWindow).
func TestCLIResumeAdmitsPermanentRunFinished(t *testing.T) {
	t.Parallel()
	runID := "test-resume-admit-permanent"
	stateDir := buildResumeLog(t, "testdata/phase2/seq.yaml", runID,
		runFinishedEvent(t, engine.OutcomePermanentFailure),
	)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, mutatedSeqPath(t)}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (digest mismatch after admit)", rc)
	}
	got := stderr.String()
	if !strings.Contains(got, "digest mismatch") {
		t.Errorf("guard did not admit a permanent_failure run: %q", got)
	}
	if strings.Contains(got, "Re-run with --force") {
		t.Errorf("guard wrongly refused a permanent_failure run: %q", got)
	}
}

// ADMIT: run.finished{rejected}.
func TestCLIResumeAdmitsRejectedRunFinished(t *testing.T) {
	t.Parallel()
	runID := "test-resume-admit-rejected"
	stateDir := buildResumeLog(t, "testdata/phase2/seq.yaml", runID,
		runFinishedEvent(t, engine.OutcomeRejected),
	)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, mutatedSeqPath(t)}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (digest mismatch after admit)", rc)
	}
	got := stderr.String()
	if !strings.Contains(got, "digest mismatch") {
		t.Errorf("guard did not admit a rejected run: %q", got)
	}
	if strings.Contains(got, "Re-run with --force") {
		t.Errorf("guard wrongly refused a rejected run: %q", got)
	}
}

// END-TO-END: a real run fails transiently (echo_step exits 1); resume admits it
// through the real guard and re-runs the uncommitted frontier to completion.
// Uses seq-retry1.yaml (echo_step retry.attempts:1) so the failed attempt does
// NOT pay real retry backoff (~3s under the default 3 attempts; runAndFinish
// hardcodes clock.System{}, so a fake clock can't be injected at the CLI boundary
// — same approach as the existing try.yaml / TestCLIRunOnTryFixture pattern).
// (Committed-step replay is pinned separately by TestCLIResumeHappyPathSkipsCommittedSteps.)
func TestCLIResumeRetryableEndToEnd(t *testing.T) {
	stateDir := t.TempDir()
	runID := "test-resume-retryable-e2e"

	// Run 1: echo_step fails transiently.
	fake1 := container.NewFake()
	fake1.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake1.ProgramExec("echo step2", container.ExecResult{ExitCode: 1}, nil) // transient (exit!=0, not declared non-retryable)
	runner1 := &cli.Runner{Backend: fake1, IDGen: &clock.Fake{IDs: []string{runID}}}
	var o1, e1 bytes.Buffer
	rc1 := runner1.Run([]string{
		"run", "--state-dir", stateDir, "--run-id", runID, "testdata/phase2/seq-retry1.yaml",
	}, &o1, &e1)
	if rc1 == cli.ExitOK {
		t.Fatalf("run 1 should have failed transiently; rc=%d stderr=%s", rc1, e1.String())
	}

	// Run 2: resume with echo_step now succeeding.
	fake2 := container.NewFake()
	fake2.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake2.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`),
	}, nil)
	fake2.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	runner2 := &cli.Runner{Backend: fake2, IDGen: &clock.Fake{}}
	var o2, e2 bytes.Buffer
	rc2 := runner2.Run([]string{
		"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq-retry1.yaml",
	}, &o2, &e2)
	if rc2 != cli.ExitOK {
		t.Fatalf("resume rc=%d, want ExitOK; stderr=%s", rc2, e2.String())
	}
	if !strings.Contains(e2.String(), "side effects may repeat") {
		t.Errorf("resume did not print the terminal-resume note: %q", e2.String())
	}
}

// buildPermanentFailureRun produces a run on disk that ends with
// run.finished{permanent_failure}. It uses a single-step workflow whose
// command exits 78 (EX_CONFIG — the spec §6 default non-retryable sentinel).
// Returns stateDir + runID + wfPath so callers resume against the SAME workflow
// file (identical digest; no duplicated YAML in each test).
func buildPermanentFailureRun(t *testing.T) (stateDir, runID, wfPath string) {
	t.Helper()
	tmp := t.TempDir()
	wfPath = filepath.Join(tmp, "fail-wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: force-test-fail
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: step1
    container: lab
    run: "./fail.sh"
`), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	runID = "test-force-perm-fail"
	stateDir = t.TempDir()
	fake := container.NewFake()
	// Exit 78 → permanent_failure via the spec §6 default retry policy.
	fake.ProgramExec("./fail.sh", container.ExecResult{ExitCode: 78}, nil)
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{IDs: []string{runID}}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "--run-id", runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitRunFailed {
		t.Fatalf("setup: expected ExitRunFailed, got %d; stderr: %s", rc, stderr.String())
	}
	// Verify log has run.finished{permanent_failure} before returning.
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	lg, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("setup OpenLog: %v", err)
	}
	defer func() { _ = lg.Close() }()
	events, err := lg.Fold()
	if err != nil {
		t.Fatalf("setup Fold: %v", err)
	}
	var gotPermanent bool
	for _, e := range events {
		if e.Type == engine.EventRunFinished {
			var d engine.RunFinishedData
			if jsonErr := json.Unmarshal(e.Data, &d); jsonErr == nil && d.Outcome == "permanent_failure" {
				gotPermanent = true
			}
		}
	}
	if !gotPermanent {
		t.Fatalf("setup: log does not contain run.finished{permanent_failure}")
	}
	return stateDir, runID, wfPath
}

// TestCLIResumeTerminalRunStillEnforcesDigestPin verifies plain resume of a
// terminally-failed run still hard-errors on a changed definition digest:
// dropping the terminal-admission gate does NOT relax pinning.
func TestCLIResumeTerminalRunStillEnforcesDigestPin(t *testing.T) {
	t.Parallel()
	stateDir, runID, wfPath := buildPermanentFailureRun(t)

	// Mutate the on-disk workflow so its digest no longer matches the digest
	// stored in run.started. We change the lab container's image digest from
	// all-zeros to all-f's — both are syntactically valid pinned OCI refs
	// (the validator only requires "@sha256:"), so the mutated file passes
	// ir.Validate but hashes differently. The failure must come from the
	// resume-side digest-mismatch check, not from a parse/validation error.
	orig, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatalf("read wf: %v", err)
	}
	mutated := strings.Replace(
		string(orig),
		"sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		1,
	)
	if mutated == string(orig) {
		t.Fatal("mutation no-op: the image digest was not found in the workflow file")
	}
	if err := os.WriteFile(wfPath, []byte(mutated), 0o600); err != nil {
		t.Fatalf("write mutated wf: %v", err)
	}

	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage (digest pin must hold on terminal-run resume); stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "digest mismatch") {
		t.Fatalf("want 'digest mismatch' error; got %q", stderr.String())
	}
	// The pin check must fire BEFORE run.resumed is appended.
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	lg, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = lg.Close() }()
	events, err := lg.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	for _, e := range events {
		if e.Type == engine.EventRunResumed {
			t.Errorf("digest-mismatch refusal on terminal-run resume must NOT append run.resumed; events: %+v", events)
		}
	}
}

// TestCLIResumeTerminalRunResumesWithoutFlag confirms a permanently-failed run
// resumes with NO flag, prints the non-fatal "side effects may repeat" note,
// and completes ExitOK once the cause is fixed (fake reprogrammed to exit 0).
func TestCLIResumeTerminalRunResumesWithoutFlag(t *testing.T) {
	t.Parallel()
	stateDir, runID, wfPath := buildPermanentFailureRun(t)

	// Reprogram the fake: the operator "fixed the cause" so step1 now exits 0.
	fakeOK := container.NewFake()
	fakeOK.ProgramExec("./fail.sh", container.ExecResult{ExitCode: 0}, nil)
	runner := &cli.Runner{Backend: fakeOK, IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"resume", "--state-dir", stateDir, runID, wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "ended in permanent_failure") || !strings.Contains(got, "side effects may repeat") {
		t.Errorf("stderr missing the terminal-resume note: %q", got)
	}
}
