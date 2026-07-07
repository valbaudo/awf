package cli_test

// F4b (AWF1065): end-to-end CLI coverage for the run-start container-less
// `run:` / docker-backend guard (cli/backend_features.go,
// checkContainerlessRunCapability), wired into cli/run.go right after backend
// resolution. Unit-level coverage of the pure walker/guard functions lives in
// cli/backend_features_test.go; these tests instead drive the real `awf run`
// path (runner.Run) to prove the wiring itself — that the guard actually
// fires before any state hits disk, for both an explicit `--backend docker`
// and a MIXED workflow that only reaches docker via auto-resolution.
//
// container.NewFake() is injected via newTestRunner for every case, including
// the "docker" ones: cli.Runner.resolveBackend returns the test-injected
// Backend regardless of the requested kind (cli/backend.go), so these tests
// exercise the real selectRunBackendForLoadedDefinition / concreteBackendKind
// string path with no docker daemon required.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

func writeBareRunOnlyWorkflow(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "bare-only.yaml")
	if err := os.WriteFile(path, []byte(`workflow: bare-only
version: 1
graph:
  - id: bare
    run: "true"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeBareRunMixedWorkflow(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "bare-mixed.yaml")
	if err := os.WriteFile(path, []byte(`workflow: bare-mixed
version: 1
containers:
  lab:
    image: oci://example.com/lab@sha256:`+strings.Repeat("0", 64)+`
graph:
  - id: withimg
    container: lab
    run: "true"
  - id: bare
    run: "true"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// (a) a bare `run:` under explicit --backend docker → AWF1065 at run-start,
// before run.id is even minted (no log file left behind).
func TestCLIRunBareRunDockerExplicitRejected(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfPath := writeBareRunOnlyWorkflow(t, t.TempDir())
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--backend", "docker", "--state-dir", stateDir, wfPath}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "AWF1065") {
		t.Errorf("stderr = %q, want AWF1065", stderr.String())
	}
	if !strings.Contains(stderr.String(), "bare") {
		t.Errorf("stderr = %q, want the offending step path %q", stderr.String(), "bare")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "runs", "test-run-1", "log")); !os.IsNotExist(err) {
		t.Fatalf("log exists after AWF1065 rejection; err = %v, want ErrNotExist", err)
	}
}

// (b) a MIXED workflow — an image-backed container alongside an unrelated
// bare `run:` step — with NO --backend flag: auto-resolution picks docker
// because of the image, and the guard (which runs AFTER that resolution)
// must still catch the bare step. Proves the guard isn't just checking the
// literal --backend flag value.
func TestCLIRunBareRunDockerMixedAutoRejected(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfPath := writeBareRunMixedWorkflow(t, t.TempDir())
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, wfPath}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "AWF1065") {
		t.Errorf("stderr = %q, want AWF1065", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "runs", "test-run-1", "log")); !os.IsNotExist(err) {
		t.Fatalf("log exists after AWF1065 rejection; err = %v, want ErrNotExist", err)
	}
}

// (c) --backend fake supports a bare `run:` (no AWF1065) — the fake backend
// creates an empty-spec handle for F4a's per-step host-workspace Create just
// like it does for any other Create call.
func TestCLIRunBareRunFakeBackendAccepted(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfPath := writeBareRunOnlyWorkflow(t, t.TempDir())
	fake := container.NewFake()
	fake.ProgramExec("true", container.ExecResult{ExitCode: 0}, nil)
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--backend", "fake", "--state-dir", stateDir, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	if strings.Contains(stderr.String(), "AWF1065") {
		t.Errorf("stderr = %q, want no AWF1065", stderr.String())
	}
}

// explicit --backend native must also accept a bare `run:` step (F4a already
// runs it host-side; F4b's guard is docker-only, so native must be a pure
// no-op here) — completing the "fake/native/auto-native pass" claim.
func TestCLIRunBareRunNativeExplicitAccepted(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfPath := writeBareRunOnlyWorkflow(t, t.TempDir())
	fake := container.NewFake()
	fake.ProgramExec("true", container.ExecResult{ExitCode: 0}, nil)
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--backend", "native", "--state-dir", stateDir, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	backendField := readRunStartedBackendField(t, stateDir, "test-run-1")
	if backendField != engine.BackendNative {
		t.Errorf("run.started.Backend = %q, want %q", backendField, engine.BackendNative)
	}
}
