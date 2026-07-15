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
	"github.com/valbaudo/awf/signal"
)

func TestCLIPauseWritesFile(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "r"
	_ = os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"pause", "--state-dir", stateDir, runID, "--reason", "inspect",
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want %d; stderr: %s", rc, cli.ExitOK, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(signal.ControlDir(stateDir, runID), "pause.json"))
	if err != nil {
		t.Fatalf("read pause.json: %v", err)
	}
	var req signal.PauseRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("parse pause.json: %v", err)
	}
	if req.Reason != "inspect" {
		t.Errorf("got %+v, want Reason=inspect", req)
	}
}

func TestCLIPauseRejectsBeforeFlag(t *testing.T) {
	// H8 fix: --before <node-path> is reserved for Phase 6; Phase 3 rejects
	// explicitly rather than silently accepting and ignoring.
	t.Parallel()
	stateDir := t.TempDir()
	runID := "r"
	_ = os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"pause", "--state-dir", stateDir, runID, "--before", "step.triage",
	}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Errorf("rc = %d, want %d (--before should be rejected in Phase 3)", rc, cli.ExitUsage)
	}
	if !strings.Contains(stderr.String(), "not yet supported in Phase 3") {
		t.Errorf("stderr missing Phase 3 deferral message: %q", stderr.String())
	}
}

func TestCLIPauseInfraOnControlWriteFailure(t *testing.T) {
	stateDir := t.TempDir()
	runID := "r"
	runDir := filepath.Join(stateDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "control"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer

	rc := runner.Run([]string{"pause", "--state-dir", stateDir, runID}, &stdout, &stderr)
	if rc != cli.ExitInfra {
		t.Fatalf("rc = %d, want ExitInfra; stderr: %s", rc, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty on failure", stdout.String())
	}
	for _, want := range []string{"write pause control file", "current UID", "owner UID"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q: %s", want, stderr.String())
		}
	}
}
