package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
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
