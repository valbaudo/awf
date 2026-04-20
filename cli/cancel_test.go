package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/signal"
)

func TestCLICancelWritesFile(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "r"
	_ = os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"cancel", "--state-dir", stateDir, runID, "--reason", "stop"}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want %d; stderr: %s", rc, cli.ExitOK, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(signal.ControlDir(stateDir, runID), "cancel.json"))
	if err != nil {
		t.Fatalf("read cancel.json: %v", err)
	}
	var req signal.CancelRequest
	_ = json.Unmarshal(data, &req)
	if req.Reason != "stop" {
		t.Errorf("Reason = %q, want stop", req.Reason)
	}
}
