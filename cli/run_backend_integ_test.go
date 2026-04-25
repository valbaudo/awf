//go:build integ

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// alpineDigest matches container/docker/backend_integ_test.go. Keeping
// the same digest means CI pulls the image once across both packages'
// integ runs.
const alpineDigest = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// Timeout constants for the integ tests. Centralized so a slow CI runner
// can be adjusted in one place.
const (
	pullTimeout              = 60 * time.Second
	stepCommitPollTimeout    = 60 * time.Second
	stepCommitPollInterval   = 100 * time.Millisecond
	runReturnAfterPauseLimit = 30 * time.Second
	resumeCompletionLimit    = 30 * time.Second
	cleanupCtxTimeout        = 30 * time.Second
)

// pullImage ensures the digest is available locally before tests construct
// Backend.Create paths that depend on it. Copied from slice-4.1's package-
// private container/docker/backend_integ_test.go:267-310 helper (not
// importable across packages). The body MUST scan the JSON status stream
// rather than io.Copy(io.Discard, reader) — pull errors surface mid-stream
// as objects with errorDetail.message OR a top-level error field; draining
// silently would swallow them and surface as a confusing "no such image"
// later at ContainerCreate. Slice 4.1's helper has an explicit warning
// comment for this exact pattern; matching it here.
//
// TODO: extract to a shared `testutil` package in a future slice if a
// third consumer appears.
func pullImage(t *testing.T, cli *dockerclient.Client, ref string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pullTimeout)
	defer cancel()
	reader, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		t.Fatalf("ImagePull(%q): %v", ref, err)
	}
	defer func() { _ = reader.Close() }()

	type pullStatus struct {
		Error       string `json:"error,omitempty"`
		ErrorDetail struct {
			Message string `json:"message"`
		} `json:"errorDetail,omitempty"`
	}
	dec := json.NewDecoder(reader)
	for {
		var s pullStatus
		if err := dec.Decode(&s); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatalf("decode pull stream for %q: %v", ref, err)
		}
		if s.Error != "" {
			t.Fatalf("pull %q: %s", ref, s.Error)
		}
		if s.ErrorDetail.Message != "" {
			t.Fatalf("pull %q: %s", ref, s.ErrorDetail.Message)
		}
	}
}

// registerComposeProjectCleanup arranges for ALL containers named
// `awf-<runID>...` to be force-removed at test end (or on test panic). This
// is the slice-4.5 minimal version of slice-4.1's full cleanupOrphans
// (~80 lines covering containers + networks + volumes); slice 4.5 only
// removes containers because (a) the slice-4.3 compose-mode tests' full
// cleanupOrphans already covers the network/volume sweep pattern for that
// package, and (b) for cli/ integ tests the leak risk is dominated by
// containers (networks/volumes auto-clean when the last container goes).
//
// Why this matters: cli/run.go's deferred Destroy fires only if the runner
// goroutine returns normally. A test panic in goroutine 2 kills the process
// before goroutine 1's defers run — compose project leaks. The t.Cleanup
// here runs even on test panic (Go testing guarantee), forcing removal.
func registerComposeProjectCleanup(t *testing.T, cli *dockerclient.Client, runID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupCtxTimeout)
		defer cancel()
		prefix := "awf-" + runID
		containers, err := cli.ContainerList(ctx, dockerContainer.ListOptions{All: true})
		if err != nil {
			t.Logf("cleanup: ContainerList: %v", err)
			return
		}
		for _, c := range containers {
			matched := false
			for _, name := range c.Names {
				// Docker prefixes container names with "/".
				if strings.HasPrefix(strings.TrimPrefix(name, "/"), prefix) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			if err := cli.ContainerRemove(ctx, c.ID, dockerContainer.RemoveOptions{Force: true}); err != nil {
				t.Logf("cleanup: ContainerRemove(%s): %v", c.ID, err)
			}
		}
	})
}

// runResult is the per-goroutine summary the integ test's runner-launch
// goroutines push back through their done channel. Named to deduplicate
// what was four anonymous-struct literals across the pause-resume test.
type runResult struct {
	rc     int
	stderr string
}

// waitForCommittedStep polls the log for a node.completed event at the
// given path. Times out per stepCommitPollTimeout.
func waitForCommittedStep(t *testing.T, stateDir, runID, path string) {
	t.Helper()
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	deadline := time.Now().Add(stepCommitPollTimeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("node.completed at path %q never landed within %v", path, stepCommitPollTimeout)
		}
		// Use state.FoldFile (read-only os.Open + scanFile) NOT
		// state.OpenLog (which truncates torn tails on open and would
		// destroy a write in-flight from goroutine 1's runner). The
		// Phase 1.5 torn-tail-recovery design is for process restart,
		// not concurrent in-process access — state.FoldFile is the
		// observer-safe entry point.
		evs, err := state.FoldFile(logPath)
		if err == nil {
			for _, e := range evs {
				if e.Type == engine.EventNodeCompleted && e.Path == path {
					return
				}
			}
		}
		time.Sleep(stepCommitPollInterval)
	}
}

// TestCLIRunCVEPipelineRealDockerToFirstAgentStep — Phase 4 exit bar:
// "Appendix A compose lab boots under real Docker."
//
// Runs `awf run --backend docker testdata/phase3/cve-pipeline.yaml`
// against real Docker. The cve-pipeline's first step (triage) is an
// agent step (uses: anthropic/claude-code) which Phase 4 still errors
// with ErrNodeNotImplemented (Phase 5 closes it).
//
// Assertion: the log carries a node.failed event at path "triage". If
// we reached triage's failure, by definition Create succeeded (cli/run.go
// fails fast on Create errors before any node runs — see the
// create-handles loop). Structural assertion replaces the original
// fragile "stderr does NOT contain 'create container'" string-grep per
// slice-4.5 plan §Major #9.
//
// The expected boot envelope (~90 seconds for pull cache miss + 4
// compose-up services) is informational only; the test relies on the
// underlying runner / Docker timeouts rather than a wrapping context.
func TestCLIRunCVEPipelineRealDockerToFirstAgentStep(t *testing.T) {
	dockerCli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}
	defer func() { _ = dockerCli.Close() }()
	pullImage(t, dockerCli, alpineDigest)

	stateDir := t.TempDir()
	// UnixNano suffix makes the runID unique across re-runs in the same
	// shell session (so a leftover from a prior failed run doesn't collide
	// with this run's OpenLogExclusive). registerComposeProjectCleanup
	// operates on the unique `awf-<runID>` container-name prefix.
	runID := fmt.Sprintf("cve-real-docker-%d", time.Now().UnixNano())
	registerComposeProjectCleanup(t, dockerCli, runID)

	runner := &cli.Runner{
		IDGen: &clock.Fake{IDs: []string{runID}},
	}
	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "--backend", "docker",
			"--input", `{"cve_id":"CVE-9999-0001"}`,
			"testdata/phase3/cve-pipeline.yaml"},
		&stdout, &stderr,
	)

	if rc == cli.ExitOK {
		t.Fatalf("rc = %d, want non-zero (first agent step should error)\nstdout: %s\nstderr: %s", rc, stdout.String(), stderr.String())
	}

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
	var sawTriageFailed bool
	for _, e := range events {
		if e.Type == engine.EventNodeFailed && e.Path == "triage" {
			var d engine.NodeFailedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("Unmarshal NodeFailedData: %v", err)
			}
			sawTriageFailed = true
			// Sanity: failure should mention "not implemented" (Phase 5).
			// Wording-only diagnostic; doesn't gate the test.
			if !strings.Contains(d.Error, "not implemented") {
				t.Logf("note: triage failed but error %q doesn't mention 'not implemented'", d.Error)
			}
			break
		}
	}
	if !sawTriageFailed {
		t.Fatalf("no node.failed event at path \"triage\" — graph didn't reach the first agent step, which means the lab compose project FAILED to boot.\nstdout: %s\nstderr: %s\nevents: %d", stdout.String(), stderr.String(), len(events))
	}
}

// TestCLIRunDockerBackendPauseResumeRoundTrip — Phase 4 exit-bar item:
// "awf run --backend docker followed by a forced crash + awf resume reads
// the Backend field from run.started correctly and continues on Docker."
//
// Substitution: "crash via cancel" is impossible (cancel is TERMINAL —
// awf resume refuses run.cancelled per cli/resume.go:116-121). "Crash via
// pause" between two code steps has a real timing race (pause halts at
// commit boundaries but ctx-cancel propagation could classify the
// cancelled step as node.failed; cli/pause's `--before` is REJECTED in
// Phase 3 per cli/pause.go:71-74 — "lands with Phase 6 obs").
//
// Solution: insert an `await:` step between step_a and step_b. The await
// BLOCKS in broker.Receive; that's the engine's natural break point.
// Pause fires while the engine is parked in the await (no in-flight step
// to cancel). Resume's signal delivery unblocks the await; step_b runs.
// Deterministic regardless of CI speed.
//
// Why the lab persists across pause-resume: cli/execute.go sets
// skipTeardown=true on ErrPaused, so the deferred Destroy doesn't fire;
// containers stay up. Resume's Backend.Create against the same compose
// project name is idempotent (compose `up` is a no-op when the project's
// services are already running). step_a's file in /tmp/awf-step-a
// persists in the same container instance, so step_b's `cat` succeeds.
//
// Why we use cli's "signal" subcommand (runner.Run) instead of
// broker.WriteSignal direct: matches the existing TestCLIRunOnSignalFixture
// pattern (cli/run_test.go:818) and exercises the production signal-
// delivery path end-to-end. Signals are buffered per AgentWorkflowFormat.md
// §4.3 — writing the signal BEFORE resume starts is safe; the broker
// stores it and the await consumes when reached.
func TestCLIRunDockerBackendPauseResumeRoundTrip(t *testing.T) {
	dockerCli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}
	defer func() { _ = dockerCli.Close() }()
	pullImage(t, dockerCli, alpineDigest)

	tmp := t.TempDir()
	composePath := filepath.Join(tmp, "compose.yml")
	if err := os.WriteFile(composePath, []byte(`
services:
  lab:
    image: `+alpineDigest+`
    command: ["sh", "-c", "sleep 86400"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: backend-pause-resume
version: 1
containers:
  lab:
    compose: ./compose.yml
    service: lab
graph:
  - id: step_a
    container: lab
    run: "echo step-a-marker > /tmp/awf-step-a"
  - id: wait_for_resume
    await: pause_marker
  - id: step_b
    container: lab
    run: "cat /tmp/awf-step-a"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	runID := fmt.Sprintf("docker-pause-resume-%d", time.Now().UnixNano())
	registerComposeProjectCleanup(t, dockerCli, runID)

	// First invocation: runs in a goroutine. step_a commits, then
	// engine blocks in await(pause_marker). The test writes pause.json;
	// controls poller fires; broker.Receive's ctx-cancel unblocks the
	// await with ErrPaused (engine), which cli/execute.go treats as a
	// clean halt (rc=ExitOK, skipTeardown=true).
	runner1 := &cli.Runner{
		IDGen: &clock.Fake{IDs: []string{runID}},
	}
	done := make(chan runResult, 1)
	go func() {
		var sout, serr bytes.Buffer
		rc := runner1.Run(
			[]string{"run", "--state-dir", stateDir, "--run-id", runID, "--backend", "docker", wfPath},
			&sout, &serr,
		)
		done <- runResult{rc: rc, stderr: serr.String()}
	}()

	// Wait for step_a's commit; once that lands, the engine is about to
	// (or already has) entered the await step.
	waitForCommittedStep(t, stateDir, runID, "step_a")

	// Pause via cli's pause subcommand (matches the production path; no
	// direct broker.WritePause). Uses default broker poll interval (the
	// CLI's pause writes the file; engine's controls goroutine polls).
	pauseRunner := &cli.Runner{IDGen: &clock.Fake{}}
	var pstdout, pstderr bytes.Buffer
	prc := pauseRunner.Run(
		[]string{"pause", "--state-dir", stateDir, runID, "--reason", "slice-4.5 pause-resume integ"},
		&pstdout, &pstderr,
	)
	if prc != cli.ExitOK {
		t.Fatalf("pause rc = %d; stderr: %s", prc, pstderr.String())
	}

	// Wait for first runner.Run to return with ErrPaused (rc=ExitOK).
	select {
	case result := <-done:
		if result.rc != cli.ExitOK {
			t.Fatalf("first run rc = %d, want ExitOK (pause should be clean); stderr: %s", result.rc, result.stderr)
		}
	case <-time.After(runReturnAfterPauseLimit):
		t.Fatal("first run did not return within timeout after pause")
	}

	// Log assertions: run.started{Backend:"docker"} + step_a's
	// node.completed + run.paused. NOT wait_for_resume or step_b
	// (await holds them deterministically).
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	fl, err := state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	events, err := fl.Fold()
	_ = fl.Close()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	var startedBackend string
	var sawStepA, sawPaused, sawAwait, sawStepB bool
	for _, e := range events {
		switch e.Type {
		case engine.EventRunStarted:
			var d engine.RunStartedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("Unmarshal run.started event: %v", err)
			}
			startedBackend = d.Backend
		case engine.EventNodeCompleted:
			switch e.Path {
			case "step_a":
				sawStepA = true
			case "wait_for_resume":
				sawAwait = true
			case "step_b":
				sawStepB = true
			}
		case engine.EventRunPaused:
			sawPaused = true
		}
	}
	if startedBackend != engine.BackendDocker {
		t.Errorf("run.started.Backend = %q, want %q", startedBackend, engine.BackendDocker)
	}
	if !sawStepA {
		t.Error("missing node.completed at path step_a after first run")
	}
	if !sawPaused {
		t.Error("missing run.paused after first run")
	}
	if sawAwait {
		t.Error("wait_for_resume committed before resume — the await didn't actually block")
	}
	if sawStepB {
		t.Error("step_b committed before resume — interpreter advanced past the await before pause")
	}

	// Pre-load the signal that will unblock the await. Signals are
	// buffered per AgentWorkflowFormat.md §4.3 ("journaled on receipt
	// even before the await is reached, consumed earliest-first per
	// name, never lost across a restart"). Writing it BEFORE resume
	// means the engine consumes it as soon as resume reaches the
	// await — no second goroutine needed.
	sigRunner := &cli.Runner{IDGen: &clock.Fake{}}
	var sstdout, sstderr bytes.Buffer
	src := sigRunner.Run(
		[]string{"signal", "--state-dir", stateDir, runID, "pause_marker"},
		&sstdout, &sstderr,
	)
	if src != cli.ExitOK {
		t.Fatalf("signal rc = %d; stderr: %s", src, sstderr.String())
	}

	// Resume with NO --backend flag. cli/resume reads Backend from log,
	// ClearPauseCancel removes pause.json (signal-file is separate, kept),
	// constructs Docker backend, runs await (consumes the queued signal),
	// runs step_b, finishes ok.
	runner2 := &cli.Runner{
		IDGen: &clock.Fake{},
	}
	resumeDone := make(chan runResult, 1)
	go func() {
		var sout, serr bytes.Buffer
		rc := runner2.Run(
			[]string{"resume", "--state-dir", stateDir, runID, wfPath},
			&sout, &serr,
		)
		resumeDone <- runResult{rc: rc, stderr: serr.String()}
	}()
	select {
	case result := <-resumeDone:
		if result.rc != cli.ExitOK {
			t.Fatalf("resume rc = %d, want ExitOK; stderr: %s", result.rc, result.stderr)
		}
	case <-time.After(resumeCompletionLimit):
		t.Fatal("resume did not complete within timeout")
	}

	// Final log assertion: step_b committed + run.finished{ok}.
	fl, err = state.OpenLog(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLog (post-resume): %v", err)
	}
	defer func() { _ = fl.Close() }()
	events, err = fl.Fold()
	if err != nil {
		t.Fatalf("Fold (post-resume): %v", err)
	}
	sawAwait, sawStepB = false, false
	var sawSignalReceived bool
	var finishedOutcome string
	for _, e := range events {
		switch e.Type {
		case engine.EventNodeCompleted:
			switch e.Path {
			case "wait_for_resume":
				sawAwait = true
			case "step_b":
				sawStepB = true
			}
		case engine.EventSignalReceived:
			sawSignalReceived = true
		case engine.EventRunFinished:
			var d engine.RunFinishedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("Unmarshal run.finished event: %v", err)
			}
			finishedOutcome = d.Outcome
		}
	}
	if !sawSignalReceived {
		t.Error("missing signal.received after resume — the buffered signal wasn't consumed by the await")
	}
	if !sawAwait {
		t.Error("missing node.completed at path wait_for_resume — await didn't commit after signal")
	}
	if !sawStepB {
		t.Error("missing node.completed at path step_b after resume — backend wasn't re-constructed correctly")
	}
	if finishedOutcome != "ok" {
		t.Errorf("run.finished outcome = %q, want \"ok\"", finishedOutcome)
	}
}
