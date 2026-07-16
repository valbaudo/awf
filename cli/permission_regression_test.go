package cli_test

import (
	"bytes"
	"fmt"
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
)

func writePermissionCodeWorkflow(t *testing.T, retryBlock string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "permission-code.yaml")
	content := `workflow: permission-code
version: 1
containers:
  lab:
    image: oci://example.com/lab@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: blocked
    container: lab
    run: "./blocked.sh"
` + retryBlock
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func permissionExecError() error {
	return &os.PathError{Op: "exec", Path: "./blocked.sh", Err: syscall.EACCES}
}

func TestCLIRunCodePermissionFailureDefaultsToOneAttemptAndNeverPrintsOK(t *testing.T) {
	t.Parallel()
	backend := container.NewFake()
	backend.ProgramExecAny(container.ExecResult{Err: permissionExecError()}, nil)
	runner := newTestRunner(t, backend)

	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--backend", "fake", "--state-dir", t.TempDir(),
		writePermissionCodeWorkflow(t, ""),
	}, &stdout, &stderr)

	if rc != cli.ExitRunFailed {
		t.Fatalf("rc = %d, want ExitRunFailed; stdout=%q stderr=%q", rc, stdout.String(), stderr.String())
	}
	if got := len(backend.Calls); got != 1 {
		t.Fatalf("dispatches = %d, want exactly 1; calls=%+v", got, backend.Calls)
	}
	if !strings.Contains(stderr.String(), "permission denied") || !strings.Contains(stderr.String(), "retryable_failure") {
		t.Fatalf("stderr missing terminal permission failure: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "retrying as") {
		t.Fatalf("one-attempt default emitted a retry notice: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "run test-run-1: ok") {
		t.Fatalf("permission failure printed success: %q", stdout.String())
	}
}

func TestCLIRunCodePermissionFailureExplicitTwoAttemptsPrintsOneRetryNotice(t *testing.T) {
	t.Parallel()
	backend := container.NewFake()
	backend.ProgramExecAny(container.ExecResult{Err: permissionExecError()}, nil)
	runner := newTestRunner(t, backend)

	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--backend", "fake", "--state-dir", t.TempDir(),
		writePermissionCodeWorkflow(t, "    retry: { attempts: 2, backoff: none }\n"),
	}, &stdout, &stderr)

	if rc != cli.ExitRunFailed {
		t.Fatalf("rc = %d, want ExitRunFailed; stdout=%q stderr=%q", rc, stdout.String(), stderr.String())
	}
	if got := len(backend.Calls); got != 2 {
		t.Fatalf("dispatches = %d, want exactly 2; calls=%+v", got, backend.Calls)
	}
	if got := strings.Count(stderr.String(), "retrying as"); got != 1 {
		t.Fatalf("retry notices = %d, want exactly 1; stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "attempt 1/2 failed") || !strings.Contains(stderr.String(), "retrying as 2/2") {
		t.Fatalf("stderr missing first-to-second attempt progress: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "run test-run-1: ok") {
		t.Fatalf("permission failure printed success: %q", stdout.String())
	}
}

func TestCLIRunInflightAgentPermissionDeniedIsPermanentWithoutRetryNotice(t *testing.T) {
	t.Parallel()
	const ref = "test/permission-denied"
	adapter := agentfake.New(ref).
		WithCaps(agent.Caps{Containerless: true}).
		Script(0, agentfake.Result{Err: fmt.Errorf("provider authentication: %w", agent.ErrPermissionDenied)})
	var registry agent.Registry
	if err := registry.Register(adapter); err != nil {
		t.Fatalf("Register: %v", err)
	}

	wfPath := filepath.Join(t.TempDir(), "agent-permission.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: agent-permission
version: 1
containers: {}
graph:
  - id: denied
    uses: test/permission-denied
`), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	runner := &cli.Runner{
		Backend:  container.NewFake(),
		Resolver: &registry,
		IDGen:    &clock.Fake{IDs: []string{"agent-permission-run"}},
	}

	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--backend", "fake", "--state-dir", t.TempDir(), wfPath}, &stdout, &stderr)

	if rc != cli.ExitRunFailed {
		t.Fatalf("rc = %d, want ExitRunFailed; stdout=%q stderr=%q", rc, stdout.String(), stderr.String())
	}
	if got := len(adapter.Calls()); got != 1 {
		t.Fatalf("agent launches = %d, want exactly 1", got)
	}
	if !strings.Contains(stderr.String(), "permanent_failure") || !strings.Contains(stderr.String(), "permission denied") {
		t.Fatalf("stderr missing permanent permission failure: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "retrying as") {
		t.Fatalf("permanent agent permission failure emitted retry notice: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "run agent-permission-run: ok") {
		t.Fatalf("agent permission failure printed success: %q", stdout.String())
	}
}
