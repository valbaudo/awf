# Resume transiently-failed runs — Scope A Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `awf resume` admit a run whose terminal rollup is `retryable_failure` (re-running its uncommitted frontier), while still refusing `permanent_failure` / `rejected` / `ok` / `cancelled`; and surface resumable runs distinctly in `awf ls`.

**Architecture:** Make the `cli/resume.go` guard outcome-aware: `run.finished.Outcome` is the sole admit/refuse authority when present, and `node.failed` is consulted only in the crash window (no `run.finished`). Two thin `engine` accessors decode the event payloads. `obs.DeriveStatus` gains a `RunResumable` status. Once admitted, the engine already re-runs the uncommitted frontier for any node kind — **no engine control-flow change in Scope A.** (Scope B, a separate plan, makes failed *map items* re-run.)

**Tech Stack:** Go 1.26; module `github.com/valbaudo/awf`. Pre-commit verification is **`make lint test`** (golangci-lint), NOT `go test + vet + gofmt`. Tests run on the fake backend (no Docker).

**Spec:** `docs/superpowers/specs/2026-06-14-resume-retryable-failure-design.md` (§4 contract, §5 Scope A, §6.6 ls status, §9 idempotency, §11 docs).

---

### Task 1: Engine accessors for `run.finished` / `node.failed` payloads

`engine.Fold` ignores both events, so the guard must decode them from the raw event slice. There is no existing accessor (`RunStartedDataFromEvents` only handles `events[0]`). Add two thin decoders that take the single matched event.

**Files:**
- Create: `engine/run_finished.go`
- Test: `engine/run_finished_test.go`

- [ ] **Step 1: Write the failing test**

`engine/run_finished_test.go`:
```go
package engine

import (
	"encoding/json"
	"testing"

	"github.com/valbaudo/awf/state"
)

func TestRunFinishedDataFromEvent(t *testing.T) {
	data, err := json.Marshal(RunFinishedData{Outcome: string(OutcomeRetryableFailure)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	d, err := RunFinishedDataFromEvent(state.Event{Type: EventRunFinished, Data: data})
	if err != nil {
		t.Fatalf("RunFinishedDataFromEvent: %v", err)
	}
	if d.Outcome != string(OutcomeRetryableFailure) {
		t.Errorf("Outcome = %q, want %q", d.Outcome, OutcomeRetryableFailure)
	}
}

func TestNodeFailedDataFromEvent(t *testing.T) {
	data, err := json.Marshal(NodeFailedData{Outcome: string(OutcomePermanentFailure), Error: "boom"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	d, err := NodeFailedDataFromEvent(state.Event{Type: EventNodeFailed, Path: "map[0].item-2", Data: data})
	if err != nil {
		t.Fatalf("NodeFailedDataFromEvent: %v", err)
	}
	if d.Outcome != string(OutcomePermanentFailure) {
		t.Errorf("Outcome = %q, want %q", d.Outcome, OutcomePermanentFailure)
	}
}

func TestRunFinishedDataFromEventBadJSON(t *testing.T) {
	if _, err := RunFinishedDataFromEvent(state.Event{Type: EventRunFinished, Data: []byte("{not json")}); err == nil {
		t.Error("want error on malformed payload, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./engine/ -run 'TestRunFinishedDataFromEvent|TestNodeFailedDataFromEvent' -v`
Expected: FAIL — `undefined: RunFinishedDataFromEvent` / `NodeFailedDataFromEvent`.

- [ ] **Step 3: Write minimal implementation**

`engine/run_finished.go`:
```go
package engine

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/state"
)

// RunFinishedDataFromEvent decodes a run.finished event's payload. engine.Fold
// ignores run.finished (it is observational), so resume must read the terminal
// outcome from the raw matched event, not from RunState. The caller passes the
// matched event — run.finished is never events[0].
func RunFinishedDataFromEvent(e state.Event) (RunFinishedData, error) {
	var d RunFinishedData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return RunFinishedData{}, fmt.Errorf("engine: unmarshal run.finished: %w", err)
	}
	return d, nil
}

// NodeFailedDataFromEvent decodes a node.failed event's payload (same rationale).
func NodeFailedDataFromEvent(e state.Event) (NodeFailedData, error) {
	var d NodeFailedData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return NodeFailedData{}, fmt.Errorf("engine: unmarshal node.failed: %w", err)
	}
	return d, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./engine/ -run 'TestRunFinishedDataFromEvent|TestNodeFailedDataFromEvent' -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add engine/run_finished.go engine/run_finished_test.go
git commit -m "feat(engine): add RunFinishedDataFromEvent / NodeFailedDataFromEvent accessors"
```

---

### Task 2: Outcome-aware resume guard (the core change)

Replace the three blanket refusal loops (`cli/resume.go:147-171`) so `run.finished.Outcome` is the sole authority when present, and `node.failed` is consulted only in the crash window. This fixes the false-refusal of compound retryable runs (a `run.finished{retryable_failure}` co-existing with a nested `node.failed{permanent_failure}` from a tolerated map item — the common case).

**Files:**
- Modify: `cli/resume.go:145-171` (the guard block)
- Test: `cli/resume_test.go` (update one existing test, add helpers + new tests)

- [ ] **Step 1: Add test helpers**

Append to `cli/resume_test.go` (these mirror the existing `buildInFlightLogForWF` / `TestCLIResumeDigestMismatchHardError` patterns; an *admitted* resume reaches the digest check, so a mutated-workflow fixture proves admission cleanly):
```go
// buildResumeLog writes a hand-crafted log under a fresh stateDir for runID:
// run.started (digest of wfPath) followed by the given terminal events. Mirrors
// the inline fixture in TestCLIResumeDigestMismatchHardError (Backend field
// omitted — resolveBackend tolerates it and reaches the digest check).
func buildResumeLog(t *testing.T, wfPath, runID string, terminal ...state.Event) string {
	t.Helper()
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "runs", runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := state.OpenBlobs(filepath.Join(stateDir, "blobs")); err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	ld, err := loader.Load(wfPath)
	if err != nil {
		t.Fatalf("loader.Load(%q): %v", wfPath, err)
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	log, err := state.OpenLogExclusive(logPath, clock.System{})
	if err != nil {
		t.Fatalf("OpenLogExclusive: %v", err)
	}
	rsd, err := json.Marshal(engine.RunStartedData{RunID: runID, WorkflowDigest: digest})
	if err != nil {
		t.Fatalf("Marshal run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: rsd}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	for _, e := range terminal {
		if err := log.Append(e); err != nil {
			t.Fatalf("Append %s: %v", e.Type, err)
		}
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return stateDir
}

func nodeFailedEvent(t *testing.T, path string, oc engine.Outcome) state.Event {
	t.Helper()
	d, err := json.Marshal(engine.NodeFailedData{Outcome: string(oc)})
	if err != nil {
		t.Fatalf("marshal node.failed: %v", err)
	}
	return state.Event{Type: engine.EventNodeFailed, Path: path, Data: d}
}

func runFinishedEvent(t *testing.T, oc engine.Outcome) state.Event {
	t.Helper()
	d, err := json.Marshal(engine.RunFinishedData{Outcome: string(oc)})
	if err != nil {
		t.Fatalf("marshal run.finished: %v", err)
	}
	return state.Event{Type: engine.EventRunFinished, Data: d}
}

// mutatedSeqPath returns a temp copy of seq.yaml with one run: line changed, so
// its digest differs from the original. Resuming against it after admission
// triggers the "digest mismatch" hard error — our proof that the guard admitted.
func mutatedSeqPath(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("testdata/phase2/seq.yaml")
	if err != nil {
		t.Fatalf("read seq.yaml: %v", err)
	}
	mutated := strings.Replace(string(src),
		"run: \"touch /tmp/awf-seq-marker\"",
		"run: \"touch /tmp/awf-seq-marker-MUTATED\"", 1)
	if mutated == string(src) {
		t.Fatal("mutation no-op")
	}
	p := filepath.Join(t.TempDir(), "seq-mutated.yaml")
	if err := os.WriteFile(p, []byte(mutated), 0o644); err != nil {
		t.Fatalf("WriteFile mutated: %v", err)
	}
	return p
}
```

- [ ] **Step 2: Write the failing admit/refuse tests**

Append to `cli/resume_test.go`:
```go
// ADMIT: run.finished{retryable_failure} co-existing with a nested permanent
// node.failed (tolerated map item). The OLD guard refused; the new guard admits
// (run.finished is sole authority) → reaches the digest check. THE regression test.
func TestCLIResumeAdmitsRetryableDespiteNestedPermanent(t *testing.T) {
	t.Parallel()
	runID := "test-resume-admit-compound"
	stateDir := buildResumeLog(t, "testdata/phase2/seq.yaml", runID,
		nodeFailedEvent(t, "map[0].item-2", engine.OutcomePermanentFailure),
		runFinishedEvent(t, engine.OutcomeRetryableFailure),
	)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, mutatedSeqPath(t)}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (digest mismatch after admit)", rc)
	}
	got := stderr.String()
	if !strings.Contains(got, "digest mismatch") {
		t.Errorf("guard did not admit (no digest-mismatch reached): %q", got)
	}
	if strings.Contains(got, "already finished") || strings.Contains(got, "Not resumable") {
		t.Errorf("guard wrongly refused a retryable run: %q", got)
	}
}

// ADMIT: crash window — node.failed{retryable_failure}, no run.finished.
func TestCLIResumeAdmitsRetryableCrashWindow(t *testing.T) {
	t.Parallel()
	runID := "test-resume-admit-crash"
	stateDir := buildResumeLog(t, "testdata/phase2/seq.yaml", runID,
		nodeFailedEvent(t, "echo_step", engine.OutcomeRetryableFailure),
	)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, mutatedSeqPath(t)}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (digest mismatch after admit)", rc)
	}
	got := stderr.String()
	if !strings.Contains(got, "digest mismatch") {
		t.Errorf("guard did not admit a retryable crash-window run: %q", got)
	}
	if strings.Contains(got, "non-transient") {
		t.Errorf("guard wrongly refused a retryable crash-window run: %q", got)
	}
}

// REFUSE: run.finished{permanent_failure}.
func TestCLIResumeRefusesPermanentRunFinished(t *testing.T) {
	t.Parallel()
	runID := "test-resume-refuse-permanent"
	stateDir := buildResumeLog(t, "testdata/phase2/seq.yaml", runID,
		runFinishedEvent(t, engine.OutcomePermanentFailure),
	)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (permanent failure)", rc)
	}
	if !strings.Contains(stderr.String(), "terminal permanent_failure") {
		t.Errorf("stderr missing 'terminal permanent_failure': %q", stderr.String())
	}
	assertNoRunResumed(t, stateDir, runID)
}

// REFUSE: run.finished{rejected}.
func TestCLIResumeRefusesRejectedRunFinished(t *testing.T) {
	t.Parallel()
	runID := "test-resume-refuse-rejected"
	stateDir := buildResumeLog(t, "testdata/phase2/seq.yaml", runID,
		runFinishedEvent(t, engine.OutcomeRejected),
	)
	runner := &cli.Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq.yaml"}, &stdout, &stderr)
	if rc == cli.ExitOK {
		t.Errorf("rc = %d, want non-zero (rejected)", rc)
	}
	if !strings.Contains(stderr.String(), "terminal rejected") {
		t.Errorf("stderr missing 'terminal rejected': %q", stderr.String())
	}
	assertNoRunResumed(t, stateDir, runID)
}

// END-TO-END: a real run fails transiently (echo_step exits 1, exhausts retries
// → run.finished{retryable_failure}); resume admits it through the real guard and
// re-runs the uncommitted frontier to completion. The committed first step is
// replayed, not re-executed. This is the strongest Scope-A proof — no map needed.
func TestCLIResumeRetryableEndToEnd(t *testing.T) {
	stateDir := t.TempDir()
	runID := "test-resume-retryable-e2e"

	// Run 1: echo_step fails transiently.
	fake1 := container.NewFake()
	fake1.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake1.ProgramExec("echo step2", container.ExecResult{ExitCode: 1}, nil) // transient (exit!=0, not declared non-retryable)
	runner1 := &cli.Runner{Backend: fake1, IDGen: &clock.Fake{IDs: []string{runID}}}
	var o1, e1 bytes.Buffer
	rc1 := runner1.Run([]string{
		"run", "--state-dir", stateDir, "--run-id", runID, "testdata/phase2/seq.yaml",
	}, &o1, &e1)
	if rc1 == cli.ExitOK {
		t.Fatalf("run 1 should have failed transiently; rc=%d stderr=%s", rc1, e1.String())
	}

	// Run 2: resume with echo_step now succeeding.
	fake2 := container.NewFake()
	fake2.ProgramExec("touch /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	fake2.ProgramExec("echo step2", container.ExecResult{
		ExitCode: 0, AWFOutput: []byte(`{"message":"step2"}`),
	}, nil)
	fake2.ProgramExec("cat /tmp/awf-seq-marker", container.ExecResult{ExitCode: 0}, nil)
	runner2 := &cli.Runner{Backend: fake2, IDGen: &clock.Fake{}}
	var o2, e2 bytes.Buffer
	rc2 := runner2.Run([]string{
		"resume", "--state-dir", stateDir, runID, "testdata/phase2/seq.yaml",
	}, &o2, &e2)
	if rc2 != cli.ExitOK {
		t.Fatalf("resume rc=%d, want ExitOK; stderr=%s", rc2, e2.String())
	}
	if !strings.Contains(e2.String(), "previously failed transiently") {
		t.Errorf("resume did not log the transient-admit notice: %q", e2.String())
	}
}
```
(The committed-first-step-is-replayed-not-re-run guarantee is already pinned by `TestCLIResumeHappyPathSkipsCommittedSteps`; this test focuses on the new admit-and-complete path. If you want a re-dispatch assertion here, check `fake2`'s recorded exec calls using whatever accessor `container.Fake` exposes — confirm its shape in `container/fake.go` first.)

- [ ] **Step 3: Update the crash-window permanent test's expected message**

In `cli/resume_test.go`, `TestCLIResumeRefusesNodeFailedInLog` (currently asserts `"terminated on a failed step"`). The new crash-window refusal message for a permanent `node.failed` is `"terminated on a non-transient failure"`. Change its assertion:
```go
	if !strings.Contains(stderrStr, "terminated on a non-transient failure") {
		t.Errorf("stderr missing 'terminated on a non-transient failure' (node.failed refusal): %q", stderrStr)
	}
```
(Leave the `!strings.Contains(stderrStr, "already finished")` precedence check unchanged — it still holds.)

- [ ] **Step 4: Run the new tests to verify they fail**

Run: `go test ./cli/ -run 'TestCLIResumeAdmits|TestCLIResumeRefuses(Permanent|Rejected)RunFinished|TestCLIResumeRefusesNodeFailedInLog|TestCLIResumeRetryableEndToEnd' -v`
Expected: FAIL — admit tests see `"already finished"` / refusal instead of `"digest mismatch"`; the permanent/rejected refuse tests see `"already finished"` (the old blanket run.finished loop) instead of the new messages; the updated node-failed test sees the old `"failed step"` wording; the end-to-end test's `resume` is refused (`"already finished"`) so `rc2 != ExitOK`.

- [ ] **Step 5: Replace the guard block**

In `cli/resume.go`, replace the three loops at lines 145-171 with:
```go
	// Step 3: terminal-event refusals, outcome-aware (spec §4 resumability
	// contract). run.finished.Outcome is the SOLE admit/refuse authority when
	// present; node.failed is consulted ONLY in the crash window (no
	// run.finished). A retryable_failure rollup can co-exist with a nested
	// permanent node.failed (a tolerated map-item body, a non-lowest-index
	// parallel branch, or a try{do:permanent,catch:retryable}) — so a standalone
	// node.failed scan would wrongly refuse a resumable run.
	var finished *engine.RunFinishedData
	for _, e := range events {
		if e.Type == engine.EventRunFinished {
			d, err := engine.RunFinishedDataFromEvent(e)
			if err != nil {
				fprintf(stderr, "awf resume: run %q has a corrupt run.finished record: %v\n", runID, err)
				return ExitUsage
			}
			finished = &d
			break
		}
	}
	// run.cancelled stays a blanket terminal refusal, BEFORE the node.failed
	// fallback: cancel-during-step writes both events; show "cancelled".
	for _, e := range events {
		if e.Type == engine.EventRunCancelled {
			fprintf(stderr, "awf resume: run %q was cancelled (run.cancelled in log). Cannot resume a cancelled run; start a new run id.\n", runID)
			return ExitUsage
		}
	}
	if finished != nil {
		switch engine.Outcome(finished.Outcome) {
		case engine.OutcomeRetryableFailure:
			fprintf(stderr, "awf resume: run %q previously failed transiently (retryable_failure); re-attempting the uncommitted frontier\n", runID)
			// ADMIT — fall through to backend wiring. Do NOT scan node.failed.
		case engine.OutcomeOK:
			fprintf(stderr, "awf resume: run %q already finished (ok). Nothing to resume.\n", runID)
			return ExitUsage
		default: // permanent_failure, rejected, empty/unknown
			fprintf(stderr, "awf resume: run %q ended with a terminal %s (not resumable); start a new run id.\n", runID, finished.Outcome)
			return ExitUsage
		}
	} else {
		// Crash window: no run.finished. node.failed is the terminal record.
		// Admit ONLY a retryable failure; refuse permanent/rejected/empty.
		for _, e := range events {
			if e.Type == engine.EventNodeFailed {
				d, err := engine.NodeFailedDataFromEvent(e)
				if err == nil && engine.Outcome(d.Outcome) == engine.OutcomeRetryableFailure {
					continue
				}
				fprintf(stderr, "awf resume: run %q terminated on a non-transient failure at path %q. Not resumable.\n", runID, e.Path)
				return ExitUsage
			}
		}
	}
```

- [ ] **Step 6: Run the full resume test suite**

Run: `go test ./cli/ -run TestCLIResume -v`
Expected: PASS — new admit/refuse tests green; `TestCLIResumeRefusesTerminalRunFinished` (run.finished{ok} via `firstRunSeq`) still passes (`"already finished"` substring preserved); `TestCLIResumeRefusesTerminalRunCancelled`, `TestCLIResumeDigestMismatchHardError`, `TestCLIResumeHappyPathSkipsCommittedSteps` unchanged.

- [ ] **Step 7: Commit**

```bash
git add cli/resume.go cli/resume_test.go
git commit -m "feat(cli): resume admits retryable_failure runs (outcome-aware guard)"
```

---

### Task 3: `RunResumable` status in `awf ls`

After Task 2, a `retryable_failure` run is resumable but `obs.DeriveStatus` collapses it to `failed`, indistinguishable from `permanent_failure`/`rejected`. Split both terminal arms so resumable runs read `resumable`.

**Files:**
- Modify: `obs/runstatus.go` (const block, doc-comment, two arms)
- Test: `obs/runstatus_test.go` (add cases)

- [ ] **Step 1: Write the failing test cases**

In `obs/runstatus_test.go`, add to the `cases` slice in `TestDeriveStatus`:
```go
		{"resumable-run-finished", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventRunFinished, "", t0.Add(time.Second), engine.RunFinishedData{Outcome: "retryable_failure"}),
		), RunResumable},
		{"failed-permanent-run-finished", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventRunFinished, "", t0.Add(time.Second), engine.RunFinishedData{Outcome: "rejected"}),
		), RunFailed},
		{"resumable-terminal-node-failed", mk(
			ev(t, engine.EventRunStarted, "", t0, engine.RunStartedData{RunID: "r"}),
			ev(t, engine.EventNodeStarted, "s1", t0.Add(time.Second), engine.NodeStartedData{Kind: "code"}),
			ev(t, engine.EventNodeFailed, "s1", t0.Add(2*time.Second), engine.NodeFailedData{Outcome: "retryable_failure", Error: "transient"}),
		), RunResumable},
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./obs/ -run TestDeriveStatus -v`
Expected: FAIL — `undefined: RunResumable`.

- [ ] **Step 3: Add the `RunResumable` const**

In `obs/runstatus.go`, add to the const block (after `RunFailed`):
```go
	RunResumable RunStatus = "resumable" // run.finished{retryable_failure} OR terminal node.failed{retryable_failure} — re-drivable via `awf resume`
```

- [ ] **Step 4: Split the `run.finished` arm**

In `DeriveStatus`, replace the `case engine.EventRunFinished:` block:
```go
		case engine.EventRunFinished:
			var d engine.RunFinishedData
			if err := json.Unmarshal(e.Data, &d); err == nil {
				switch engine.Outcome(d.Outcome) {
				case engine.OutcomeOK:
					return RunFinished
				case engine.OutcomeRetryableFailure:
					return RunResumable
				}
			}
			return RunFailed
```

- [ ] **Step 5: Split the trailing `node.failed` arm**

Replace the trailing block (`if n := len(events); n > 0 && events[n-1].Type == engine.EventNodeFailed { return RunFailed }`):
```go
	if n := len(events); n > 0 && events[n-1].Type == engine.EventNodeFailed {
		var d engine.NodeFailedData
		if err := json.Unmarshal(events[n-1].Data, &d); err == nil &&
			engine.Outcome(d.Outcome) == engine.OutcomeRetryableFailure {
			return RunResumable
		}
		return RunFailed
	}
```

- [ ] **Step 6: Update the precedence doc-comment**

In the doc-comment block (runstatus.go:32-39), update the precedence lines:
```go
//	run.finished{retryable_failure}       → resumable
//	run.finished{permanent/rejected}      → failed
//	last event node.failed{retryable}     → resumable
//	last event node.failed{permanent}     → failed
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./obs/ -run TestDeriveStatus -v`
Expected: PASS (existing + 3 new cases). `cli/ls.go` and `ui/runs.go` emit `string(status)` and need no change.

- [ ] **Step 8: Commit**

```bash
git add obs/runstatus.go obs/runstatus_test.go
git commit -m "feat(obs): RunResumable status for retryable_failure runs"
```

---

### Task 4: Documentation — co-land the contract (no code; verify with `make lint test`)

The man page is the stable format contract; the resumability rule must land with the code, not as a trailing pass. Route man-page edits through the `updating-the-manual` skill, using exactly the content below.

**Files:**
- Modify: `man/awf-workflow.5.md` (CHECKPOINTING AND RESUME; EXTERNAL EFFECTS AND IDEMPOTENCY)
- Modify: `man/awf.1.md` (`awf resume`; `awf ls` vocabulary)
- Modify: `README.md` (resume row, if it claims failed runs aren't resumable)
- Modify: `cli/resume.go` (stale comment cleanup, if any references remain)

- [ ] **Step 1: `man/awf-workflow.5.md` — resumability contract**

In the **Resume** definition-list entry (after the deterministic-replay sentence), add:
> A run is resumable iff its terminal rollup is `retryable_failure` (the log holds `run.finished{retryable_failure}`, or — in the crash window between `failStep` and `run.finished` — only `node.failed{retryable_failure}`). `ok`, `permanent_failure`, `rejected`, and `cancelled` are not resumable.

In the **Cancellation** entry, add a one-clause cross-reference: "(see Resume for the full terminal-outcome resumability rule)."

- [ ] **Step 2: `man/awf-workflow.5.md` — EXTERNAL EFFECTS AND IDEMPOTENCY (resume paragraph)**

Append to that section:
> Resuming a `retryable_failure` run re-executes a frontier that already ran to its full retry budget; this is the same at-least-once exposure crash-resume accepts. `idempotency_key` is a hint the external system enforces (injected as `AWF_IDEMPOTENCY_KEY` / `Idempotency-Key`), never engine dedup; agent autonomous effects remain outside the guarantee. There is no resume-attempt cap — a flapping transient fault re-fails each manual resume; the operator is the only bound.

- [ ] **Step 3: `man/awf.1.md` — resume + ls**

`awf resume` first sentence: change "Re-enter an interrupted run" → "Re-enter an interrupted run, or a run that terminated transiently (`retryable_failure`), re-running the transiently-failed frontier."
`awf ls` status vocabulary list (line ~219): add `resumable` — "`resumable` (failed transiently; re-drivable with `awf resume`)" and keep `failed` for permanent/rejected.

- [ ] **Step 4: README + stale comment**

Update the README "how AWF is different" resume row if it asserts failed-runs-aren't-resumable. Grep `cli/resume.go` for "Phase 3's try/catch" — the rewritten guard (Task 2) removed it; confirm none remain: `grep -n "Phase 3" cli/resume.go` → no output.

- [ ] **Step 5: Verify and commit**

Run: `make lint test`
Expected: PASS (docs don't break the build; confirms Tasks 1-3 are still green together).
```bash
git add man/awf-workflow.5.md man/awf.1.md README.md
git commit -m "docs: document retryable_failure resumability contract + ls status"
```

---

## Self-Review

- **Spec coverage:** §4 contract → Task 4 Step 1; §5.1 guard → Task 2; §5 accessors → Task 1; §6.6 ls status → Task 3; §9 idempotency → Task 4 Step 2; §11 docs → Task 4. Scope A's "node-kind-agnostic frontier" (§5.2) needs no code (engine already re-runs the uncommitted frontier once admitted) — covered by the existing `TestCLIResumeHappyPathSkipsCommittedSteps` plus the new admit tests; a composite-node conformance test is in the Scope B plan (it shares the resume harness work). No gaps.
- **Type consistency:** `RunFinishedDataFromEvent`/`NodeFailedDataFromEvent` (Task 1) are used verbatim in Task 2 and Task 3. `engine.Outcome(...)` comparison, `OutcomeRetryableFailure`/`OutcomePermanentFailure`/`OutcomeRejected`/`OutcomeOK` spellings match `engine/runstate.go`. `RunResumable` const (Task 3) used consistently. Error substrings asserted in tests match the guard's `fprintf` strings exactly (`"terminal permanent_failure"`, `"terminal rejected"`, `"terminated on a non-transient failure"`, `"previously failed transiently"`).
- **Placeholders:** none.
