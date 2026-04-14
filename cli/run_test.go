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

func newTestRunner(t *testing.T, fake *container.Fake) *cli.Runner {
	t.Helper()
	return &cli.Runner{
		Backend: fake,
		IDGen:   &clock.Fake{IDs: []string{"test-run-1"}},
	}
}

func TestCLIRunOnSeqFixture(t *testing.T) {
	if _, err := os.Stat("testdata/phase2/seq.yaml"); err != nil {
		t.Skip("testdata fixture not yet present (lands in Task 7)")
	}
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{
		ExitCode: 0, Stdout: []byte("created marker\n"),
	}, nil)
	fake.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, Stdout: []byte("step2\n"),
		AWFOutput: []byte(`{"message":"step2"}`),
	}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{
		ExitCode: 0, Stdout: []byte("end-of-seq\n"),
	}, nil)

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := newTestRunner(t, fake)
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "testdata/phase2/seq.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want %d (ExitOK)\nstderr: %s", rc, cli.ExitOK, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"[touch_marker] created marker", "[echo_step] step2", "[cat_marker] end-of-seq"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q; got %q", want, out)
		}
	}

	logPath := filepath.Join(stateDir, "runs", "test-run-1", "log")
	fl, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = fl.Close() }()
	events, err := fl.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var completed, finished int
	var finishedOutcome string
	for _, e := range events {
		switch e.Type {
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
	if completed != 3 {
		t.Errorf("node.completed events = %d, want 3", completed)
	}
	if finished != 1 || finishedOutcome != "ok" {
		t.Errorf("run.finished = %d events outcome=%q, want 1/ok", finished, finishedOutcome)
	}
}

func TestCLIRunOnIfFixture(t *testing.T) {
	if _, err := os.Stat("testdata/phase2/if.yaml"); err != nil {
		t.Skip("testdata fixture not yet present (lands in Task 7)")
	}
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("echo triage", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"web_exploitable":true}`),
		Stdout:    []byte("triaged\n"),
	}, nil)
	fake.ProgramExec("echo exploitable", container.ExecResult{
		ExitCode: 0, Stdout: []byte("exploit time\n"),
	}, nil)

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := newTestRunner(t, fake)
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "testdata/phase2/if.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	logPath := filepath.Join(stateDir, "runs", "test-run-1", "log")
	fl, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = fl.Close() }()
	events, err := fl.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var bt *engine.BranchTakenData
	for _, e := range events {
		if e.Type == engine.EventBranchTaken {
			var d engine.BranchTakenData
			_ = json.Unmarshal(e.Data, &d)
			bt = &d
		}
	}
	if bt == nil || bt.Which != "then" {
		t.Errorf("branch.taken Which = %v, want 'then'", bt)
	}
}

func TestCLIRunOnLoopFixture(t *testing.T) {
	if _, err := os.Stat("testdata/phase2/loop.yaml"); err != nil {
		t.Skip("testdata fixture not yet present (lands in Task 7)")
	}
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("echo iter", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"counter":1}`),
		Stdout:    []byte("iter\n"),
	}, nil)

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := newTestRunner(t, fake)
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "testdata/phase2/loop.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	logPath := filepath.Join(stateDir, "runs", "test-run-1", "log")
	fl, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = fl.Close() }()
	events, err := fl.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var iters int
	for _, e := range events {
		if e.Type == engine.EventLoopIter {
			iters++
		}
	}
	if iters != 3 {
		t.Errorf("loop.iter events = %d, want 3", iters)
	}
}

func TestCLIRunNonexistentFileIsExitUsage(t *testing.T) {
	t.Parallel()
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "/no/such/file.yaml"}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Errorf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
}

func TestCLIRunRefusesExistingRunIDLog(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("./noop.sh", container.ExecResult{ExitCode: 0}, nil)

	tmp := t.TempDir()
	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: refuse-existing
version: 1
containers:
  lab: { image: "oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000" }
graph:
  - id: noop
    container: lab
    run: "./noop.sh"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	r1 := newTestRunner(t, fake)
	var stdout1, stderr1 bytes.Buffer
	rc1 := r1.Run([]string{"run", "--state-dir", stateDir, "--run-id", "collision-id", wfPath}, &stdout1, &stderr1)
	if rc1 != cli.ExitOK {
		t.Fatalf("first run: rc = %d, want ExitOK; stderr: %s", rc1, stderr1.String())
	}

	fake2 := container.NewFake()
	fake2.ProgramExec("./noop.sh", container.ExecResult{ExitCode: 0}, nil)
	r2 := newTestRunner(t, fake2)
	var stdout2, stderr2 bytes.Buffer
	rc2 := r2.Run([]string{"run", "--state-dir", stateDir, "--run-id", "collision-id", wfPath}, &stdout2, &stderr2)
	if rc2 != cli.ExitUsage {
		t.Errorf("second run: rc = %d, want ExitUsage (run.id collision); stderr: %s", rc2, stderr2.String())
	}
	if !strings.Contains(stderr2.String(), "already exists") && !strings.Contains(stderr2.String(), "exist") {
		t.Errorf("stderr missing 'exists' hint; got %q", stderr2.String())
	}
}

func TestCLIRunInputSchemaValidationFails(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()

	tmp := t.TempDir()
	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: input-typed
version: 1
input:
  type: object
  additionalProperties: false
  required: [cve_id]
  properties:
    cve_id: { type: string }
containers:
  lab: { image: "oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000" }
graph:
  - id: noop
    container: lab
    run: "./noop.sh"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run",
		"--state-dir", stateDir,
		"--input", `{"cve_id": 42}`,
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Errorf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if len(fake.Calls) != 0 {
		t.Errorf("fake.Calls len = %d, want 0 (input failed validation, run never started)", len(fake.Calls))
	}
	if _, err := os.Stat(filepath.Join(stateDir, "runs", "test-run-1", "log")); !os.IsNotExist(err) {
		t.Errorf("orphan log file exists at runs/test-run-1/log; err = %v, want fs.ErrNotExist", err)
	}
}

func TestCLIRunRejectsInputWhenNoSchema(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()

	tmp := t.TempDir()
	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: no-input
version: 1
containers:
  lab: { image: "oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000" }
graph:
  - id: noop
    container: lab
    run: "./noop.sh"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run",
		"--state-dir", stateDir,
		"--input", `{"anything":"goes"}`,
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Errorf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "declares no input schema") {
		t.Errorf("stderr missing the 'no input schema' explanation; got %q", stderr.String())
	}
	if len(fake.Calls) != 0 {
		t.Errorf("fake.Calls len = %d, want 0 (rejection happens pre-engine)", len(fake.Calls))
	}
}

func TestCLIRunValidationFailureIsExitInvalid(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "bad.yaml")
	if err := os.WriteFile(bad, []byte(`
workflow: bad
version: 1
containers:
  lab: { image: "oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000" }
graph:
  - id: dup
    container: lab
    run: "echo 1"
  - id: dup
    container: lab
    run: "echo 2"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", bad}, &stdout, &stderr)
	if rc != cli.ExitInvalid {
		t.Errorf("rc = %d, want ExitInvalid; stderr: %s", rc, stderr.String())
	}
}

func TestCLIRunStepFailureIsExitRunFailed(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("./fail.sh", container.ExecResult{ExitCode: 78}, nil)

	tmp := t.TempDir()
	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: failer
version: 1
containers:
  lab: { image: "oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000" }
graph:
  - id: fail
    container: lab
    run: "./fail.sh"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, wfPath}, &stdout, &stderr)
	if rc != cli.ExitRunFailed {
		t.Errorf("rc = %d, want ExitRunFailed; stderr: %s", rc, stderr.String())
	}
	logPath := filepath.Join(stateDir, "runs", "test-run-1", "log")
	fl, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = fl.Close() }()
	events, err := fl.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var rf *engine.RunFinishedData
	for _, e := range events {
		if e.Type == engine.EventRunFinished {
			var d engine.RunFinishedData
			_ = json.Unmarshal(e.Data, &d)
			rf = &d
		}
	}
	if rf == nil || rf.Outcome != "permanent_failure" {
		t.Errorf("run.finished outcome = %v, want permanent_failure", rf)
	}
}

func TestCLIRunInputFlagIsBlobsPutAndScopeable(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("echo CVE-2024-9999", container.ExecResult{
		ExitCode: 0, Stdout: []byte("cve-9999\n"),
	}, nil)

	tmp := t.TempDir()
	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: with-input
version: 1
input:
  type: object
  additionalProperties: false
  required: [cve_id]
  properties:
    cve_id: { type: string }
containers:
  lab: { image: "oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000" }
graph:
  - id: echo_cve
    container: lab
    run: "echo {{ input.cve_id }}"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run",
		"--state-dir", stateDir,
		"--input", `{"cve_id":"CVE-2024-9999"}`,
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Run != "echo CVE-2024-9999" {
		t.Errorf("fake.Calls[0].Run = %q, want substituted 'echo CVE-2024-9999'", fake.Calls[0].Run)
	}
}

func TestCLIRunMintsRunIDViaIDGenWhenNotOverridden(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("./noop.sh", container.ExecResult{ExitCode: 0}, nil)

	tmp := t.TempDir()
	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: minty
version: 1
containers:
  lab: { image: "oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000" }
graph:
  - id: noop
    container: lab
    run: "./noop.sh"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "runs", "test-run-1", "log")); err != nil {
		t.Errorf("run dir not at expected path: %v", err)
	}
}

func TestCLIRunIDOverrideWins(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("./noop.sh", container.ExecResult{ExitCode: 0}, nil)

	tmp := t.TempDir()
	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: idtest
version: 1
containers:
  lab: { image: "oci://example.com/img@sha256:0000000000000000000000000000000000000000000000000000000000000000" }
graph:
  - id: noop
    container: lab
    run: "./noop.sh"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{
		"run", "--state-dir", stateDir, "--run-id", "my-explicit-id",
		wfPath,
	}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "runs", "my-explicit-id", "log")); err != nil {
		t.Errorf("run dir not at overridden path: %v", err)
	}
}
