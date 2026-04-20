package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
)

func TestCLISignalWritesFile(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "test-run-1"
	// Create the run dir so signal accepts.
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"signal", "--state-dir", stateDir, runID, "human_review",
		"--payload", `{"approved":true}`,
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want %d; stderr: %s", rc, cli.ExitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "signal human_review (seq 1)") {
		t.Errorf("stdout: %q", stdout.String())
	}
	// File on disk.
	sigPath := filepath.Join(stateDir, "runs", runID, "control", "signal-human_review-1.json")
	data, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatalf("read signal file: %v", err)
	}
	if string(data) != `{"approved":true}` {
		t.Errorf("signal payload = %q", data)
	}
}

func TestCLISignalRejectsUnknownRunID(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"signal", "--state-dir", stateDir, "no-such-run", "name",
	}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero", rc)
	}
	if !strings.Contains(stderr.String(), "no run with id") {
		t.Errorf("stderr: %q", stderr.String())
	}
}

func TestCLISignalRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "r"
	_ = os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"signal", "--state-dir", stateDir, runID, "name", "--payload", "{not json",
	}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Errorf("rc = %d, want %d", rc, cli.ExitUsage)
	}
	if !strings.Contains(stderr.String(), "not valid JSON") {
		t.Errorf("stderr: %q", stderr.String())
	}
}
