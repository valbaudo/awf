package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claude"
	agentfake "github.com/valbaudo/awf/agent/fake"
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

// TestCLIRunGateRejectedIsExitRunFailed: a top-level gate that exhausts
// max_attempts without passing terminates the run as OutcomeRejected. The CLI
// must surface that as a rejected run (ExitRunFailed) with a "rejected"
// message — NOT mislabel it "internal error"/ExitUsage (the switch's default
// arm is for the empty-outcome interpreter-bug case only).
func TestCLIRunGateRejectedIsExitRunFailed(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("echo gen1", container.ExecResult{
		ExitCode: 0, Stdout: []byte("gen1\n"),
	}, []container.IOChunk{{Stream: "stdout", Data: []byte("gen1\n")}})
	// verified:false → until false → max_attempts:1 → rejected on attempt 1.
	fake.ProgramExec("echo eval1", container.ExecResult{
		ExitCode:  0,
		Stdout:    []byte("eval1\n"),
		AWFOutput: []byte(`{"verified":false,"feedback":"nope"}`),
	}, []container.IOChunk{{Stream: "stdout", Data: []byte("eval1\n")}})

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := newTestRunner(t, fake)
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "testdata/phase3/gate-reject.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitRunFailed {
		t.Fatalf("rc = %d, want %d (ExitRunFailed for a rejected gate)\nstderr: %s", rc, cli.ExitRunFailed, stderr.String())
	}
	if !strings.Contains(stderr.String(), "rejected") {
		t.Errorf("stderr should name the rejection; got %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "internal error") {
		t.Errorf("a gate rejection must NOT be mislabeled 'internal error'; got %q", stderr.String())
	}
	// The durable record carries the distinct 'rejected' outcome.
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
			var dd engine.RunFinishedData
			_ = json.Unmarshal(e.Data, &dd)
			finishedOutcome = dd.Outcome
		}
	}
	if finishedOutcome != "rejected" {
		t.Errorf("run.finished outcome = %q, want %q", finishedOutcome, "rejected")
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
			"--backend", "docker",
			"--input", `{"cve_id":"CVE-2024-0000"}`,
			"testdata/phase3/cve-pipeline.yaml"},
		&stdout, &stderr,
	)
	// Non-OK exit; output mentions agent / not implemented OR the new
	// run-start resolver error (slice 5.1 wires resolveRuntimes before
	// engine dispatch, so an unregistered adapter now fails earlier).
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (agent step should error)", rc)
	}
	combined := stdout.String() + stderr.String()
	// Slice 5.3 Task 19 update: with production --agent-env wiring, the Claude
	// adapter is now registered by default, so the prior "no adapter registered"
	// branch is replaced by a real version-resolution failure (the fake backend
	// has no "claude --version" programmed). Both branches are acceptable
	// agent-error markers.
	if !strings.Contains(combined, "not implemented") &&
		!strings.Contains(combined, "no adapter registered") &&
		!strings.Contains(combined, "version resolution") {
		t.Errorf("output missing agent-error marker: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// recordingBackend wraps container.Fake and captures ContainerSpec values
// passed to Create. It delegates all other calls to the underlying Fake.
type recordingBackend struct {
	*container.Fake
	mu      sync.Mutex
	created []container.ContainerSpec
}

func (r *recordingBackend) Create(ctx context.Context, spec container.ContainerSpec) (container.Handle, error) {
	r.mu.Lock()
	r.created = append(r.created, spec)
	r.mu.Unlock()
	return r.Fake.Create(ctx, spec)
}

// TestCLIRunPropagatesComposeBytesToBackend verifies that when a workflow uses
// a compose-mode container, the CLI passes compose bytes (and the compose path
// + service name) to the Backend's Create call — i.e., the ContainerSpecFor
// wiring flows end-to-end from loader → cli/run → engine.LocalDispatcher →
// Backend.Create.
func TestCLIRunPropagatesComposeBytesToBackend(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("echo hello", container.ExecResult{
		ExitCode: 0, Stdout: []byte("hello\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("hello\n")},
	})
	rec := &recordingBackend{Fake: fake}

	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	runner := &cli.Runner{Backend: rec, IDGen: &clock.Fake{IDs: []string{"test-compose-wiring"}}}
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "--backend", "docker", "testdata/phase4/cli-compose-wiring.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	if len(rec.created) == 0 {
		t.Fatal("Backend.Create was never called")
	}
	var labSpec container.ContainerSpec
	var found bool
	for _, s := range rec.created {
		if s.Name == "lab" {
			labSpec = s
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no spec recorded for container \"lab\"; got %d specs: %+v", len(rec.created), rec.created)
	}
	if labSpec.Compose == nil {
		t.Errorf("spec.Compose = nil; want compose bytes (compose-mode container)")
	}
	if !bytes.Contains(labSpec.Compose, []byte("services:")) {
		t.Errorf("spec.Compose missing \"services:\" marker; got %q", labSpec.Compose)
	}
	if labSpec.ComposePath != "lab/compose.yml" {
		t.Errorf("spec.ComposePath = %q, want \"lab/compose.yml\"", labSpec.ComposePath)
	}
	if labSpec.Service != "runner" {
		t.Errorf("spec.Service = %q, want \"runner\"", labSpec.Service)
	}
	if labSpec.Image != "" {
		t.Errorf("spec.Image = %q, want empty (compose-mode)", labSpec.Image)
	}
}

func TestCLIRunWritesBackendNativeOnRunStartedByDefault(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`),
	}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "testdata/phase2/seq.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	backendField := readRunStartedBackendField(t, stateDir, "test-run-1")
	if backendField != engine.BackendNative {
		t.Errorf("run.started.Backend = %q, want %q (default)", backendField, engine.BackendNative)
	}
}

func TestCLIRunBackendDockerFlagWritesBackendDocker(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`),
	}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "--backend", "docker", "testdata/phase2/seq.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	backendField := readRunStartedBackendField(t, stateDir, "test-run-1")
	if backendField != engine.BackendDocker {
		t.Errorf("run.started.Backend = %q, want %q", backendField, engine.BackendDocker)
	}
}

func TestCLIRunBackendNativeFlagWritesBackendNative(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`),
	}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "--backend", "native", "testdata/phase2/seq.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	backendField := readRunStartedBackendField(t, stateDir, "test-run-1")
	if backendField != engine.BackendNative {
		t.Errorf("run.started.Backend = %q, want %q", backendField, engine.BackendNative)
	}
}

func TestCLIRunRejectsComposeWithNativeBackend(t *testing.T) {
	t.Parallel()
	// Decision 1 + H8 fix: --backend native with a compose-mode container
	// must fail-fast at the CLI layer (post-loader.Load, pre-dispatch),
	// not mid-run at Backend.Create.
	stateDir := t.TempDir()
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	// Use the CVE-pipeline fixture which declares a compose-mode container.
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "--backend", "native", "testdata/phase3/cve-pipeline.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitUsage {
		t.Errorf("rc = %d, want ExitUsage (compose+native rejected)", rc)
	}
	if !strings.Contains(stderr.String(), "compose-mode") {
		t.Errorf("stderr missing 'compose-mode': %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "native") {
		t.Errorf("stderr missing 'native': %s", stderr.String())
	}
}

func TestCLIRunBackendFakeFlagWritesBackendFake(t *testing.T) {
	t.Parallel()
	fake := container.NewFake()
	fake.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`),
	}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake)
	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "--backend", "fake", "testdata/phase2/seq.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	backendField := readRunStartedBackendField(t, stateDir, "test-run-1")
	if backendField != engine.BackendFake {
		t.Errorf("run.started.Backend = %q, want %q", backendField, engine.BackendFake)
	}
}

func TestCLIRunBackendInvalidValueIsExitUsage(t *testing.T) {
	t.Parallel()
	runner := newTestRunner(t, container.NewFake())
	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{"run", "--backend", "containerd", "testdata/phase2/seq.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitUsage {
		t.Errorf("rc = %d, want ExitUsage; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "containerd") {
		t.Errorf("stderr missing offending kind: %q", stderr.String())
	}
}

// readRunStartedBackendField is a test helper that opens the log at
// <stateDir>/runs/<runID>/log, folds it, finds the run.started event,
// and returns its Backend field.
func readRunStartedBackendField(t *testing.T, stateDir, runID string) string {
	t.Helper()
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
	for _, e := range events {
		if e.Type == engine.EventRunStarted {
			var d engine.RunStartedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("Unmarshal run.started: %v", err)
			}
			return d.Backend
		}
	}
	t.Fatalf("no run.started event in log %q", logPath)
	return ""
}

func TestCLIRun_AgentStepFixturePopulatesRunStartedRuntimes(t *testing.T) {
	// Fixture: a tiny workflow with one AgentStep using a fake adapter.
	// The test asserts that run.started's Runtimes field is populated correctly.
	t.Skip("Phase 5 slice 5.2 ships the AgentStep dispatcher; this test is a placeholder. Once slice 5.2 lands and AgentStep is no longer ErrNodeNotImplemented, this test runs the fixture end-to-end and inspects the run.started event for Runtimes.")
}

func TestCLIRun_NoAgentSteps_RuntimesIsAbsent(t *testing.T) {
	// Any existing Phase 2-4 fixture has no `uses:` steps — Runtimes should be
	// absent from the run.started JSON. This test is here to lock that
	// invariant (additive extension didn't break pre-Phase-5 logs).
	t.Skip("Inspect run.started JSON of any existing fixture run (e.g. testdata/phase2/seq.yaml). Assert no \"runtimes\" key in the JSON. Implementation deferred — relies on a helper that opens the log file and re-parses the first event's JSON.")
}

func TestCLIRun_AgentEnvFlag_DefaultPopulatesResolver(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-fixture")
	tmpDir := t.TempDir()
	wfPath := writeMinimalWorkflow(t, tmpDir)
	stateDir := filepath.Join(tmpDir, ".awf")

	fake := container.NewFake()
	fake.ProgramExec("true", container.ExecResult{ExitCode: 0}, nil)
	var stdout, stderr bytes.Buffer
	r := &cli.Runner{
		IDGen:   &clock.Fake{IDs: []string{"agent-env-default-run"}},
		Backend: fake,
		// NOT setting Resolver — buildAgentRegistry path triggers.
	}
	exit := r.Run([]string{"run", "--state-dir", stateDir, "--backend", "fake", wfPath}, &stdout, &stderr)
	if exit != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", exit, cli.ExitOK, stderr.String())
	}
	if r.Resolver == nil {
		t.Error("Resolver still nil after Run; want populated by --agent-env default")
	}
}

func TestCLIRun_AgentEnvFlag_EmptyValue_NoClaudeAdapter(t *testing.T) {
	tmpDir := t.TempDir()
	wfPath := writeMinimalWorkflow(t, tmpDir)
	stateDir := filepath.Join(tmpDir, ".awf")

	fake := container.NewFake()
	fake.ProgramExec("true", container.ExecResult{ExitCode: 0}, nil)
	var stdout, stderr bytes.Buffer
	r := &cli.Runner{
		IDGen:   &clock.Fake{IDs: []string{"agent-env-empty-run"}},
		Backend: fake,
	}
	exit := r.Run([]string{"run", "--state-dir", stateDir, "--backend", "fake", "--agent-env", "", wfPath}, &stdout, &stderr)
	if exit != cli.ExitOK {
		t.Fatalf("exit = %d, stderr=%s", exit, stderr.String())
	}
	if r.Resolver == nil {
		t.Fatal("Resolver nil after Run with --agent-env=''; want non-nil but empty Registry")
	}
	if reg, ok := r.Resolver.(*agent.Registry); ok {
		if _, found := reg.Lookup(claude.AdapterRef); found {
			t.Error("Claude adapter registered with --agent-env=''; want absent")
		}
	}
}

func TestCLIRun_WorkflowEnv_ExtendsAgentEnvAllowlist(t *testing.T) {
	// Gap-1: the workflow's top-level env: names extend the --agent-env allowlist.
	// Proof by isolation: pass --agent-env "" (empty base, so no adapter would be
	// registered) but declare env: [ANTHROPIC_API_KEY] in the workflow with the var
	// present in the host. The claude adapter must end up registered — which can only
	// happen if ld.Workflow.Env flowed through mergeWorkflowEnv into buildAgentRegistry.
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-fixture")
	tmpDir := t.TempDir()
	wfPath := filepath.Join(tmpDir, "wf.yaml")
	content := `workflow: wf-env
version: 1
env: [ANTHROPIC_API_KEY]
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: noop
    container: lab
    run: "true"
`
	if err := os.WriteFile(wfPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	stateDir := filepath.Join(tmpDir, ".awf")

	fake := container.NewFake()
	fake.ProgramExec("true", container.ExecResult{ExitCode: 0}, nil)
	var stdout, stderr bytes.Buffer
	r := &cli.Runner{
		IDGen:   &clock.Fake{IDs: []string{"wf-env-run"}},
		Backend: fake,
		// NOT setting Resolver — the production buildAgentRegistry path runs.
	}
	exit := r.Run([]string{"run", "--state-dir", stateDir, "--backend", "fake", "--agent-env", "", wfPath}, &stdout, &stderr)
	if exit != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", exit, cli.ExitOK, stderr.String())
	}
	reg, ok := r.Resolver.(*agent.Registry)
	if !ok {
		t.Fatalf("Resolver type = %T, want *agent.Registry", r.Resolver)
	}
	if _, found := reg.Lookup(claude.AdapterRef); !found {
		t.Error("claude adapter absent despite workflow env: [ANTHROPIC_API_KEY] with --agent-env=''; workflow env: did not extend the allowlist")
	}
}

func TestRunReleasesRunLockOnExit(t *testing.T) {
	fake := container.NewFake()
	fake.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("echo step2", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`)}, nil)
	fake.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)

	stateDir := t.TempDir()
	runner := newTestRunner(t, fake) // IDGen mints "test-run-1"
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("run rc = %d, stderr: %s", rc, stderr.String())
	}

	lockPath := filepath.Join(stateDir, "runs", "test-run-1", "run.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("run.lock not created (acquire never happened): %v", err)
	}
	// The run finished and Release fired ⇒ a fresh exclusive acquire must succeed.
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open run.lock: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("run.lock still held after run exit: %v", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// TestRunRejectsSnapshotWorkspaceOnNativeBackend exercises the slice 7.1
// capability guard (cli/snapshotguard.go): a container declaring
// snapshot: workspace cannot run on the native backend, which advertises
// container.SnapshotNone. The guard must fail fast with ExitUsage and a
// message naming "snapshot: workspace". No backend is injected so the real
// native backend is constructed (its Capabilities() is what the guard reads).
func TestRunRejectsSnapshotWorkspaceOnNativeBackend(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wf := filepath.Join(t.TempDir(), "ws.yaml")
	if err := os.WriteFile(wf, []byte(`workflow: ws
version: 1
containers:
  lab: { image: oci://example.com/x@sha256:`+strings.Repeat("0", 64)+`, snapshot: workspace }
graph:
  - id: s
    container: lab
    run: "true"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	// Backend is left nil so the real native backend is constructed (its
	// Capabilities() == SnapshotNone is what the guard reads). IDGen is set so
	// the run-id mint step (which precedes backend construction) doesn't nil-panic.
	r := &cli.Runner{IDGen: &clock.Fake{IDs: []string{"test-run-1"}}}
	rc := r.Run([]string{"run", "--backend", "native", "--state-dir", stateDir, wf}, &out, &errb)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stderr: %s", rc, errb.String())
	}
	if !strings.Contains(errb.String(), "snapshot: workspace") {
		t.Errorf("expected a capability-guard message, got: %s", errb.String())
	}
}

func writeMinimalWorkflow(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "wf.yaml")
	content := `workflow: minimal
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: noop
    container: lab
    run: "true"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

// newRegistryWith builds an *agent.Registry containing fk; fails the test on
// Register error. Used by T8 tests below.
func newRegistryWith(t *testing.T, fk *agentfake.Fake) *agent.Registry {
	t.Helper()
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return &reg
}

// writeContinuesWorkflow writes a 2-step agent workflow where the second step
// declares continues: the first, using the given adapter ref. Both steps
// share the same container "lab" (needed for non-containerless adapters).
func writeContinuesWorkflow(t *testing.T, dir, adapterRef string) string {
	t.Helper()
	path := filepath.Join(dir, "continues-wf.yaml")
	content := `workflow: continues-test
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: draft
    uses: ` + adapterRef + `
    container: lab
  - id: refine
    uses: ` + adapterRef + `
    container: lab
    continues: draft
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write continues workflow: %v", err)
	}
	return path
}

// writeContinuesWorkflowContainerless writes a 2-step agent workflow where
// both steps are containerless (no container: field) and declare continues:.
// For use with a Containerless+Threaded adapter such as awf/llm.
func writeContinuesWorkflowContainerless(t *testing.T, dir, adapterRef string) string {
	t.Helper()
	path := filepath.Join(dir, "continues-containerless-wf.yaml")
	content := `workflow: continues-containerless-test
version: 1
graph:
  - id: draft
    uses: ` + adapterRef + `
  - id: refine
    uses: ` + adapterRef + `
    continues: draft
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write containerless continues workflow: %v", err)
	}
	return path
}

// TestRun_ContinuesAgainstNonThreadedAdapter_FailsFast is the T8 end-to-end:
// `awf run` rejects a continues: step whose adapter is NOT Threaded, exits
// ExitUsage, and writes the ErrThreadedRequired message to stderr — before
// any log file is created on disk.
func TestRun_ContinuesAgainstNonThreadedAdapter_FailsFast(t *testing.T) {
	t.Parallel()
	fk := agentfake.New("anthropic/claude-code") // default Caps: Threaded false
	reg := newRegistryWith(t, fk)

	tmp := t.TempDir()
	wfPath := writeContinuesWorkflow(t, tmp, "anthropic/claude-code")
	stateDir := t.TempDir()

	r := &cli.Runner{
		Backend:  container.NewFake(),
		IDGen:    &clock.Fake{IDs: []string{"test-threaded-guard-run"}},
		Resolver: reg,
	}
	var stdout, stderr bytes.Buffer
	rc := r.Run([]string{"run", "--backend", "fake", "--state-dir", stateDir, wfPath}, &stdout, &stderr)
	if rc != cli.ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage (%d); stderr: %s", rc, cli.ExitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not support engine-threaded conversations") {
		t.Fatalf("stderr = %q, want ErrThreadedRequired message containing 'does not support engine-threaded conversations'", stderr.String())
	}
	// Guard fires BEFORE the log is opened: no state should be on disk.
	if _, err := os.Stat(filepath.Join(stateDir, "runs", "test-threaded-guard-run", "log")); !os.IsNotExist(err) {
		t.Errorf("orphan log exists after threaded-guard rejection; err = %v, want ErrNotExist", err)
	}
}

// TestRun_ContinuesAgainstThreadedAdapter_OK confirms that a continues: step
// whose adapter IS Threaded (awf/llm) passes the guard and at least reaches
// engine dispatch (the fake has no agent programmed so it will fail later,
// but the guard itself must NOT fire).
func TestRun_ContinuesAgainstThreadedAdapter_OK(t *testing.T) {
	t.Parallel()
	fk := agentfake.New("awf/llm").WithCaps(agent.Caps{Containerless: true, Threaded: true})
	reg := newRegistryWith(t, fk)

	tmp := t.TempDir()
	wfPath := writeContinuesWorkflowContainerless(t, tmp, "awf/llm")
	stateDir := t.TempDir()

	r := &cli.Runner{
		Backend:  container.NewFake(),
		IDGen:    &clock.Fake{IDs: []string{"test-threaded-ok-run"}},
		Resolver: reg,
	}
	var stdout, stderr bytes.Buffer
	rc := r.Run([]string{"run", "--backend", "fake", "--state-dir", stateDir, wfPath}, &stdout, &stderr)
	// The guard passes — run proceeds. The fake adapter will eventually fail
	// because no agent step is programmed, so we allow any non-ExitUsage
	// code. The important assertion is that the guard did NOT fire.
	if rc == cli.ExitUsage && strings.Contains(stderr.String(), "does not support engine-threaded conversations") {
		t.Fatalf("Threaded adapter incorrectly rejected by guard; stderr: %s", stderr.String())
	}
}
