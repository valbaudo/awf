package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// S5 exit-code mapping: setup failures on the state-dir side that AWF owns exit
// ExitInfra (3), distinct from ExitUsage (2) for bad input. These tests force each
// reclassified site deterministically (filesystem type conflicts, a held flock, a
// programmed backend.Create fault) — no Docker daemon required.
//
// Deterministically covered here: run open-blobs, run create-container, run
// mkdir-run-dir, run open-live-home, run run-lock-held, resume open-blobs, resume
// run-lock-held (resume_test.go), trace open-blobs, ls read-runs-dir. The remaining
// reclassified sites are one-line const flips reached only past heavier preconditions
// and are verified by inspection: backend-construct (run/resume — native.New defers
// FS errors to Create, so unreachable without a real daemon), run open-log I/O
// (non-fs.ErrExist), resume open-live-home / create-restore (need a digest-matching
// in-flight log), and the ls lock-probe error branch.

const seqImage = "oci://example.com/runner@sha256:" +
	"0000000000000000000000000000000000000000000000000000000000000000"

// writeMinimalRunLog creates an openable log holding a single run.started event at
// <stateDir>/runs/<id>/log so resume/trace reach their state-dir-side setup steps.
func writeMinimalRunLog(t *testing.T, stateDir, id string) {
	t.Helper()
	runDir := filepath.Join(stateDir, "runs", id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lg, err := state.OpenLog(filepath.Join(runDir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	rsd, _ := json.Marshal(engine.RunStartedData{RunID: id, Backend: engine.BackendFake})
	if err := lg.Append(state.Event{Type: engine.EventRunStarted, Data: rsd}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	_ = lg.Sync()
	_ = lg.Close()
}

// plantFile writes a regular file where a directory is expected, so the next
// MkdirAll/OpenBlobs/ReadDir on that path fails with a non-ErrNotExist error.
func plantFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunOpenBlobsFailureExitsInfra(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	plantFile(t, filepath.Join(stateDir, "blobs"))
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitInfra {
		t.Fatalf("run with unopenable blobs: rc = %d, want ExitInfra (3); stderr: %s", rc, stderr.String())
	}
}

func TestRunCreateContainerFailureExitsInfra(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.FailCreateForImage(seqImage)
	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitInfra {
		t.Fatalf("run with backend.Create failing: rc = %d, want ExitInfra (3); stderr: %s", rc, stderr.String())
	}
}

func TestRunOpenLiveHomeFailureExitsInfra(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	// openLiveHomeRoot → live.OpenRoot does MkdirAll(<stateDir>/live); a regular
	// file there fails it (reached after blobs + backend + capabilities succeed).
	plantFile(t, filepath.Join(stateDir, "live"))
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitInfra {
		t.Fatalf("run with live-home as a file: rc = %d, want ExitInfra (3); stderr: %s", rc, stderr.String())
	}
}

func TestRunMkdirRunDirFailureExitsInfra(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	// runs/ is a regular file → MkdirAll(runs/<id>) fails when claiming the run dir.
	plantFile(t, filepath.Join(stateDir, "runs"))
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitInfra {
		t.Fatalf("run with runs/ as a file: rc = %d, want ExitInfra (3); stderr: %s", rc, stderr.String())
	}
}

func TestRunLockHeldExitsInfra(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runID := "lk-run"
	runDir := filepath.Join(stateDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Hold the run lock as if another process were driving this id.
	lf, err := os.OpenFile(filepath.Join(runDir, "run.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lf.Close() }()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{IDs: []string{runID}}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitInfra {
		t.Fatalf("run of a lock-held id: rc = %d, want ExitInfra (3); stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already active") {
		t.Errorf("expected 'already active' message, got: %s", stderr.String())
	}
}

func TestResumeOpenBlobsFailureExitsInfra(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	writeMinimalRunLog(t, stateDir, "blob-fail")
	plantFile(t, filepath.Join(stateDir, "blobs"))
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, "blob-fail", "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitInfra {
		t.Fatalf("resume with unopenable blobs: rc = %d, want ExitInfra (3); stderr: %s", rc, stderr.String())
	}
}

func TestTraceOpenBlobsFailureExitsInfra(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	writeMinimalRunLog(t, stateDir, "trace-blob")
	plantFile(t, filepath.Join(stateDir, "blobs"))
	var stdout, stderr bytes.Buffer
	rc := cli.Run([]string{"trace", "--state-dir", stateDir, "--capture-content", "trace-blob"}, &stdout, &stderr)
	if rc != cli.ExitInfra {
		t.Fatalf("trace --capture-content with unopenable blobs: rc = %d, want ExitInfra (3); stderr: %s", rc, stderr.String())
	}
}

func TestLSReadRunsDirFailureExitsInfra(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	// runs/ is a regular file → os.ReadDir errors with a non-ErrNotExist error.
	plantFile(t, filepath.Join(stateDir, "runs"))
	var stdout, stderr bytes.Buffer
	rc := cli.Run([]string{"ls", "--state-dir", stateDir}, &stdout, &stderr)
	if rc != cli.ExitInfra {
		t.Fatalf("ls with runs/ as a file: rc = %d, want ExitInfra (3); stderr: %s", rc, stderr.String())
	}
}

// TestRunIDCollisionStaysUsage locks the carve-out: a run-id that already exists on
// disk (fs.ErrExist) is a usage conflict the user resolves by picking another id — it
// stays ExitUsage (2), NOT ExitInfra (3).
func TestRunIDCollisionStaysUsage(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	id := "collide"
	// Pre-create the run dir + log so OpenLogExclusive sees fs.ErrExist.
	writeMinimalRunLog(t, stateDir, id)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{IDs: []string{id}}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "--run-id", id, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("run with colliding run-id: rc = %d, want ExitUsage (2); stderr: %s", rc, stderr.String())
	}
}
