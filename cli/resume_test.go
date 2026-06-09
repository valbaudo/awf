package cli_test

import (
	"bytes"
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
	if !strings.Contains(stderr.String(), "already finished") {
		t.Errorf("stderr missing 'already finished': %q", stderr.String())
	}
}

func TestCLIResumeRefusesNodeFailedInLog(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "test-resume-node-failed"

	// Hand-craft a log with run.started + node.failed and NO run.finished
	// so the node.failed refusal can be tested in ISOLATION (the
	// run.finished refusal fires first when both are present). This
	// models a SIGKILL crash after node.failed but before run.finished —
	// a real scenario the resume primitive must reject.
	//
	// Slice 2.6 Design question 5: refusal precedence is run.finished
	// before node.failed before digest mismatch. This test pins the
	// node.failed branch by removing the run.finished short-circuit.
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
		t.Errorf("rc = %d, want non-zero (node.failed in log)", rc)
	}
	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "terminated on a failed step") {
		t.Errorf("stderr missing 'terminated on a failed step' (node.failed refusal): %q", stderrStr)
	}
	if strings.Contains(stderrStr, "already finished") {
		t.Errorf("run.finished refusal fired instead of node.failed (precedence inversion): %q", stderrStr)
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
	digest, err := ld.Workflow.ComputeDigest(ld.ComposeFiles, ld.Assets)
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

func TestCLIResumeRefusesTerminalRunCancelled(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "test-resume-cancelled"
	// Hand-craft a log with run.started + run.cancelled.
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
		t.Errorf("rc = %d, want non-zero", rc)
	}
	if !strings.Contains(stderr.String(), "cancelled") {
		t.Errorf("stderr missing 'cancelled': %q", stderr.String())
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
	digest, _ := ld.Workflow.ComputeDigest(ld.ComposeFiles, ld.Assets)
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
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-fixture")
	stateDir := t.TempDir()
	runID := "test-resume-workflow-env"

	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	// Digest is computed from the env:-bearing fixture — the env names fold in.
	ld, err := loader.Load("testdata/phase2/seq-with-env.yaml")
	if err != nil {
		t.Fatalf("Load fixture: %v", err)
	}
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		t.Fatalf("fixture invalid: %v", diags)
	}
	if len(ld.Workflow.Env) == 0 {
		t.Fatal("fixture lost its env: declaration on load")
	}
	digest, err := ld.Workflow.ComputeDigest(ld.ComposeFiles, ld.Assets)
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
	fake.ProgramExec("echo step2", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`)}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	r := &cli.Runner{Backend: fake, IDGen: &clock.Fake{IDs: []string{"unused"}}}
	var stdout, stderr bytes.Buffer
	_ = r.Run([]string{"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq-with-env.yaml"}, &stdout, &stderr)
	// The digest matched (env: folded symmetrically) so resume did NOT refuse;
	// the reordered registry build then populated the Resolver.
	if strings.Contains(stderr.String(), "digest mismatch") {
		t.Fatalf("resume refused on digest mismatch with env: present — env names folded asymmetrically: %s", stderr.String())
	}
	if r.Resolver == nil {
		t.Error("Resolver still nil after resume of an env:-bearing workflow; reordered registry build did not run")
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
	digest, err := ld.Workflow.ComputeDigest(ld.ComposeFiles, ld.Assets)
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
	if r.Resolver == nil {
		t.Error("Resolver still nil after resume; want populated by buildAgentRegistry with default allowlist")
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
	digest, err := ld.Workflow.ComputeDigest(ld.ComposeFiles, ld.Assets)
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
