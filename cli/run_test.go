package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/signal"
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
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{
		ExitCode: 0, Stdout: []byte("created marker\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("created marker\n")},
	})
	fake.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, Stdout: []byte("step2\n"),
		AWFOutput: []byte(`{"message":"step2"}`),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("step2\n")},
	})
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{
		ExitCode: 0, Stdout: []byte("end-of-seq\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("end-of-seq\n")},
	})

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

	// Live-tap output: each step's stdout was prefixed with [step.id].
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

func TestCLIRunOnTryFixture(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	// setup runs unconditionally.
	fake.ProgramExec("echo setup", container.ExecResult{
		ExitCode: 0, Stdout: []byte("setup\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("setup\n")},
	})
	// try-step exits 1 — retry.attempts:1 in the fixture means one attempt only,
	// so the fake needs exactly one programmed result. ExitCode:1 is a generic
	// nonzero → retryable_failure → try.catch absorbs it.
	fake.ProgramExec("exit 1", container.ExecResult{ExitCode: 1}, nil)
	// catch-step runs because try.do failed.
	fake.ProgramExec("echo caught", container.ExecResult{
		ExitCode: 0, Stdout: []byte("caught\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("caught\n")},
	})
	// finally-step always runs.
	fake.ProgramExec("echo finally", container.ExecResult{
		ExitCode: 0, Stdout: []byte("finally\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("finally\n")},
	})
	// after runs because try.catch absorbed the failure, so the run continues ok.
	fake.ProgramExec("echo after", container.ExecResult{
		ExitCode: 0, Stdout: []byte("after\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("after\n")},
	})

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := newTestRunner(t, fake)
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "testdata/phase3/try.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want %d (ExitOK); try.catch should absorb the failure\nstderr: %s", rc, cli.ExitOK, stderr.String())
	}

	// Live-tap output must include catch-step, finally-step, and after outputs.
	out := stdout.String()
	for _, want := range []string{"caught", "finally", "after"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, out)
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
	var finishedOutcome string
	for _, e := range events {
		if e.Type == engine.EventRunFinished {
			var d engine.RunFinishedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal run.finished: %v", err)
			}
			finishedOutcome = d.Outcome
		}
	}
	if finishedOutcome != "ok" {
		t.Errorf("run.finished outcome = %q, want %q", finishedOutcome, "ok")
	}
}

func TestCLIRunOnSkipFixture(t *testing.T) {
	t.Parallel()
	// No containers used — skip never invokes the dispatcher.
	fake := container.NewFake()

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := newTestRunner(t, fake)
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "testdata/phase3/skip.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want %d (ExitOK)\nstderr: %s", rc, cli.ExitOK, stderr.String())
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
	// loop{max_iters:3, body:[skip]} must produce 3 loop.iter events
	// (each skipped iter records loop.iter per Phase 3 §B design question 2)
	// and 3 node.skipped events (one per skipped iter, observational).
	var iters, skipped int
	var finishedOutcome string
	for _, e := range events {
		switch e.Type {
		case engine.EventLoopIter:
			iters++
		case engine.EventNodeSkipped:
			skipped++
		case engine.EventRunFinished:
			var d engine.RunFinishedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal run.finished: %v", err)
			}
			finishedOutcome = d.Outcome
		}
	}
	if iters != 3 {
		t.Errorf("loop.iter events = %d, want 3", iters)
	}
	if skipped != 3 {
		t.Errorf("node.skipped events = %d, want 3 (one per skipped iter)", skipped)
	}
	if finishedOutcome != "ok" {
		t.Errorf("run.finished outcome = %q, want %q", finishedOutcome, "ok")
	}
}

func TestCLIRunOnParallelFixture(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("echo pb0", container.ExecResult{
		ExitCode: 0, Stdout: []byte("pb0\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("pb0\n")},
	})
	fake.ProgramExec("echo pb1", container.ExecResult{
		ExitCode: 0, Stdout: []byte("pb1\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("pb1\n")},
	})
	fake.ProgramExec("echo pb2", container.ExecResult{
		ExitCode: 0, Stdout: []byte("pb2\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("pb2\n")},
	})
	fake.ProgramExec("echo after", container.ExecResult{
		ExitCode: 0, Stdout: []byte("after\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("after\n")},
	})

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := newTestRunner(t, fake)
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "testdata/phase3/parallel.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want %d (ExitOK)\nstderr: %s", rc, cli.ExitOK, stderr.String())
	}

	// Live-tap output: each branch's stdout was prefixed with [step.id].
	// Order is non-deterministic (concurrent goroutines) — only assert
	// presence, not ordering.
	out := stdout.String()
	for _, want := range []string{"[pb0] pb0", "[pb1] pb1", "[pb2] pb2", "[after] after"} {
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
	if completed != 4 {
		t.Errorf("node.completed events = %d, want 4 (pb0 + pb1 + pb2 + after)", completed)
	}
	if finished != 1 || finishedOutcome != "ok" {
		t.Errorf("run.finished = %d events outcome=%q, want 1/ok", finished, finishedOutcome)
	}

	// Verify Seq monotonicity — branch commits interleave by Seq through
	// the serializingLog wrapper (design §C).
	var lastSeq uint64
	for _, e := range events {
		if e.Seq <= lastSeq {
			t.Errorf("non-monotonic Seq: e.Seq=%d after lastSeq=%d", e.Seq, lastSeq)
		}
		lastSeq = e.Seq
	}
}

func TestCLIRunOnMapFixture(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("./process.sh apple 0", container.ExecResult{
		ExitCode: 0, Stdout: []byte("apple\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("apple\n")},
	})
	fake.ProgramExec("./process.sh banana 1", container.ExecResult{
		ExitCode: 0, Stdout: []byte("banana\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("banana\n")},
	})
	fake.ProgramExec("./process.sh cherry 2", container.ExecResult{
		ExitCode: 0, Stdout: []byte("cherry\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("cherry\n")},
	})
	fake.ProgramExec("echo after", container.ExecResult{
		ExitCode: 0, Stdout: []byte("after\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("after\n")},
	})

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := newTestRunner(t, fake)
	rc := runner.Run(
		[]string{
			"run", "--state-dir", stateDir,
			"--input", `{"items":["apple","banana","cherry"]}`,
			"testdata/phase3/map.yaml",
		},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want %d (ExitOK)\nstderr: %s", rc, cli.ExitOK, stderr.String())
	}

	// Live-tap output: each item's stdout was prefixed with [process].
	out := stdout.String()
	for _, want := range []string{"[process] apple", "[process] banana", "[process] cherry", "[after] after"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q; got %q", want, out)
		}
	}

	// Inspect log: 4 node.completed (3 items + 1 after), 3 map.item events,
	// 1 run.finished{outcome: ok}.
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
	var completed, mapItems, finished int
	var finishedOutcome string
	for _, e := range events {
		switch e.Type {
		case engine.EventNodeCompleted:
			completed++
		case engine.EventMapItem:
			mapItems++
		case engine.EventRunFinished:
			finished++
			var d engine.RunFinishedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal run.finished: %v", err)
			}
			finishedOutcome = d.Outcome
		}
	}
	if completed != 4 {
		t.Errorf("node.completed events = %d, want 4 (3 items + after)", completed)
	}
	if mapItems != 3 {
		t.Errorf("map.item events = %d, want 3", mapItems)
	}
	if finished != 1 || finishedOutcome != "ok" {
		t.Errorf("run.finished = %d events outcome=%q, want 1/ok", finished, finishedOutcome)
	}
}

func TestCLIRunOnSignalFixture(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("echo prep", container.ExecResult{
		ExitCode: 0, Stdout: []byte("prep\n"),
	}, nil)
	fake.ProgramExec("echo \"after\"", container.ExecResult{
		ExitCode: 0, Stdout: []byte("after\n"),
	}, nil)

	stateDir := t.TempDir()
	runID := "test-signal-run"
	// H3 fix: inject WithPollInterval(time.Millisecond) so the broker polls
	// fast in tests. Production default is 100ms; test wants ~1ms.
	runner := &cli.Runner{
		Backend:       fake,
		IDGen:         &clock.Fake{IDs: []string{runID}},
		BrokerOptions: []signal.BrokerOption{signal.WithPollInterval(time.Millisecond)},
	}

	runDone := make(chan struct {
		rc     int
		stderr string
	}, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		rc := runner.Run(
			[]string{"run", "--state-dir", stateDir, "--run-id", runID,
				"testdata/phase3/signal.yaml"},
			&stdout, &stderr,
		)
		runDone <- struct {
			rc     int
			stderr string
		}{rc, stderr.String()}
	}()

	// Coordinate via the run dir's existence. Bounded wait (≤ 2s).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(stateDir, "runs", runID)); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Write the signal.
	var sigStdout, sigStderr bytes.Buffer
	sigRC := runner.Run([]string{
		"signal", "--state-dir", stateDir, runID, "human_review",
		"--payload", `{"approved":true}`,
	}, &sigStdout, &sigStderr)
	if sigRC != cli.ExitOK {
		t.Fatalf("awf signal: rc = %d, stderr: %s", sigRC, sigStderr.String())
	}

	// With WithPollInterval(time.Millisecond), the await unblocks within a
	// few ms. 2s timeout is ample.
	select {
	case result := <-runDone:
		if result.rc != cli.ExitOK {
			t.Fatalf("awf run: rc = %d, stderr: %s", result.rc, result.stderr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awf run did not complete within 2s of signal delivery")
	}

	// Verify the log has signal.received + 3 node.completed (prep, approve, after).
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	fl, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = fl.Close() }()
	events, err := fl.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var sawSignalReceived, completed int
	var finishedOutcome string
	for _, e := range events {
		switch e.Type {
		case engine.EventSignalReceived:
			sawSignalReceived++
		case engine.EventNodeCompleted:
			completed++
		case engine.EventRunFinished:
			var d engine.RunFinishedData
			_ = json.Unmarshal(e.Data, &d)
			finishedOutcome = d.Outcome
		}
	}
	if sawSignalReceived != 1 {
		t.Errorf("signal.received events = %d, want 1", sawSignalReceived)
	}
	if completed != 3 {
		t.Errorf("node.completed events = %d, want 3 (prep + approve + after)", completed)
	}
	if finishedOutcome != "ok" {
		t.Errorf("run.finished outcome = %q, want %q", finishedOutcome, "ok")
	}
}

func TestCLIRunOnGateFixture(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("echo gen1", container.ExecResult{
		ExitCode: 0, Stdout: []byte("gen1\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("gen1\n")},
	})
	// Eval step's AWFOutput is the typed verdict — engine validates against
	// the output_schema on commit, then the gate executor reads
	// step.eval1.Outputs to evaluate `until`. With verified:true, gate passes.
	fake.ProgramExec("echo eval1", container.ExecResult{
		ExitCode:  0,
		Stdout:    []byte("eval1\n"),
		AWFOutput: []byte(`{"verified":true,"feedback":"good"}`),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("eval1\n")},
	})
	fake.ProgramExec("echo after", container.ExecResult{
		ExitCode: 0, Stdout: []byte("after\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("after\n")},
	})

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := newTestRunner(t, fake)
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "testdata/phase3/gate.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want %d (ExitOK)\nstderr: %s", rc, cli.ExitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"[gen1] gen1", "[eval1] eval1", "[after] after"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q; got %q", want, out)
		}
	}

	// Inspect log: gen1 + eval1 + after committed; exactly one gate.attempt
	// event with attempt_outcome:attempt_passed; one run.finished{ok}.
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
	var completed, gateAttempts, finished int
	var lastAttemptOutcome string
	var finishedOutcome string
	for _, e := range events {
		switch e.Type {
		case engine.EventNodeCompleted:
			completed++
		case engine.EventGateAttempt:
			gateAttempts++
			var d engine.GateAttemptData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal gate.attempt: %v", err)
			}
			lastAttemptOutcome = d.AttemptOutcome
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
		t.Errorf("node.completed events = %d, want 3 (gen1 + eval1 + after)", completed)
	}
	if gateAttempts != 1 {
		t.Errorf("gate.attempt events = %d, want 1", gateAttempts)
	}
	if lastAttemptOutcome != engine.AttemptPassed {
		t.Errorf("gate.attempt.attempt_outcome = %q, want %q", lastAttemptOutcome, engine.AttemptPassed)
	}
	if finished != 1 || finishedOutcome != "ok" {
		t.Errorf("run.finished: count=%d outcome=%q; want 1/ok", finished, finishedOutcome)
	}
}

func TestCLIValidateCVEPipelineFixture(t *testing.T) {
	t.Parallel()
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{"validate", "testdata/phase3/cve-pipeline.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want %d; stderr: %s", rc, cli.ExitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cve-pipeline") {
		t.Errorf("stdout missing workflow id: %q", stdout.String())
	}
}

func TestCLIRunCVEPipelineErrorsAtFirstAgentStep(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	stateDir := t.TempDir()
	runner := &cli.Runner{Backend: fake, IDGen: &clock.Fake{IDs: []string{"test-cve"}}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir,
			"--input", `{"cve_id":"CVE-2024-0000"}`,
			"testdata/phase3/cve-pipeline.yaml"},
		&stdout, &stderr,
	)
	// Non-OK exit; output mentions agent / not implemented.
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (agent step should error)", rc)
	}
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "not implemented") {
		t.Errorf("output missing 'not implemented': stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
