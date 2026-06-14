# resume --force Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `awf resume --force` so an operator can re-enter a terminally-failed run (`permanent_failure` / `rejected` / `cancelled`), replay committed work, and re-run the uncommitted frontier — with all pins still enforced.

**Architecture:** Three layers. (1) A pure `resumeAdmission` guard helper widens the admitted terminal-outcome set under `--force`. (2) Admission alone makes a failed *node* and a *cancelled* run productive for free — the interpreter already re-runs the uncommitted frontier. (3) One engine change — a gate-budget reset in `engine/gate.go`, reached via a new `RunOptions.ForceResume` threaded onto `interpreterContext` — makes a *rejected* gate re-run from a fresh attempt allotment. Pins (definition digest, runtime drift) are untouched.

**Tech Stack:** Go 1.26; `engine` (interpreter/gate/events), `cli` (resume command), fake container backend for tests; `make lint test`.

**Spec:** `docs/superpowers/specs/2026-06-14-resume-force-design.md`.

> **COORDINATION (read first).** This plan is the sibling of the in-flight resume-retryable-failure scope-b effort (branch `worktree-feat+resume-retryable-failure`). Both touch the resume guard region in `cli/resume.go` and both introduce `engine.RunFinishedDataFromEvent` / `engine.NodeFailedDataFromEvent`. This plan implements them so it is **self-contained and testable on its own**. At merge: keep ONE copy of the accessors, and reconcile to ONE guard whose admit set is `retryable_failure` (force-independent, the retryable effort's job) ∪ `{permanent_failure, rejected, cancelled}` (force-only, this plan's job). `resumeAdmission(force=false)` here deliberately preserves the **current** behavior (refuse every terminal marker) so this plan does **not** implement retryable admission. Each task that touches a shared point repeats this note.

---

## File Structure

- `engine/events.go` (modify) — add `RunFinishedDataFromEvent`, `NodeFailedDataFromEvent` accessors.
- `engine/events_test.go` (modify) — accessor tests.
- `cli/resume_admission.go` (create) — pure `resumeAdmission` guard helper.
- `cli/resume_admission_test.go` (create) — table tests for the helper.
- `engine/interpreter.go` (modify) — add `RunOptions.ForceResume`; thread `forceResume` into the `ictx` built in `Run`.
- `engine/interpreter_context.go` (modify) — add `forceResume bool` field.
- `engine/gate.go` (modify) — `runGate` test wrapper gains a `forceResume` param; `runGateWithContext` extends the attempt ceiling under force for an exhausted gate.
- `engine/gate_test.go` (modify) — append `false` to existing `runGate(...)` callers; add the reset test.
- `cli/resume.go` (modify) — add `--force` flag; replace the three terminal-refusal loops with a `resumeAdmission` call; print the force warning; thread force → `runAndFinish`.
- `cli/execute.go` (modify) — `runAndFinish` gains a `forceResume bool` param → `RunOptions.ForceResume`.
- `cli/run.go` (modify) — pass `false` at its `runAndFinish` call.
- `cli/resume_test.go` (modify) — guard/admission integration + pins-enforced-under-force + force-resume-completes tests.
- `man/awf.1.md` (modify) — document `--force`.

---

## Task 1: Event accessors `RunFinishedDataFromEvent` / `NodeFailedDataFromEvent`

> COORDINATION: shared with the retryable effort. If these already exist on merge, drop this task's additions and keep one copy.

**Files:**
- Modify: `engine/events.go` (after `RunStartedDataFromEvents`)
- Test: `engine/events_test.go`

- [ ] **Step 1: Write the failing test**

Add to `engine/events_test.go`:

```go
func TestRunFinishedDataFromEvent(t *testing.T) {
	e := state.Event{Type: EventRunFinished, Data: []byte(`{"outcome":"permanent_failure"}`)}
	d, err := RunFinishedDataFromEvent(e)
	if err != nil {
		t.Fatalf("RunFinishedDataFromEvent: %v", err)
	}
	if d.Outcome != "permanent_failure" {
		t.Fatalf("Outcome = %q, want permanent_failure", d.Outcome)
	}
	if _, err := RunFinishedDataFromEvent(state.Event{Type: EventRunFinished, Data: []byte(`{`)}); err == nil {
		t.Fatal("expected error on corrupt run.finished payload")
	}
}

func TestNodeFailedDataFromEvent(t *testing.T) {
	e := state.Event{Type: EventNodeFailed, Data: []byte(`{"outcome":"rejected","error":"x"}`)}
	d, err := NodeFailedDataFromEvent(e)
	if err != nil {
		t.Fatalf("NodeFailedDataFromEvent: %v", err)
	}
	if d.Outcome != "rejected" {
		t.Fatalf("Outcome = %q, want rejected", d.Outcome)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./engine/ -run 'TestRunFinishedDataFromEvent|TestNodeFailedDataFromEvent' -count=1`
Expected: FAIL — `undefined: RunFinishedDataFromEvent` / `NodeFailedDataFromEvent`.

- [ ] **Step 3: Implement the accessors**

Add to `engine/events.go` (mirror the existing `RunStartedDataFromEvents`):

```go
// RunFinishedDataFromEvent unmarshals a run.finished event's payload. Thin
// accessor used by the resume guard (cli/resume.go) to read the terminal rollup.
func RunFinishedDataFromEvent(e state.Event) (RunFinishedData, error) {
	var d RunFinishedData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return RunFinishedData{}, fmt.Errorf("engine: unmarshal run.finished: %w", err)
	}
	return d, nil
}

// NodeFailedDataFromEvent unmarshals a node.failed event's payload. Used by the
// resume guard's crash-window branch (no run.finished present).
func NodeFailedDataFromEvent(e state.Event) (NodeFailedData, error) {
	var d NodeFailedData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return NodeFailedData{}, fmt.Errorf("engine: unmarshal node.failed: %w", err)
	}
	return d, nil
}
```

If `engine/events.go` does not already import `encoding/json` / `fmt`, they are already used elsewhere in the file (`RunStartedDataFromEvents`), so no import change is needed.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./engine/ -run 'TestRunFinishedDataFromEvent|TestNodeFailedDataFromEvent' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add engine/events.go engine/events_test.go
git commit -m "feat(engine): RunFinishedDataFromEvent/NodeFailedDataFromEvent accessors"
```

---

## Task 2: `resumeAdmission` guard helper

A pure function so the guard is unit-testable without driving the full `cliResume`.

**Files:**
- Create: `cli/resume_admission.go`
- Test: `cli/resume_admission_test.go`

- [ ] **Step 1: Write the failing test**

Create `cli/resume_admission_test.go`:

```go
package cli

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func ev(t string, data string) state.Event { return state.Event{Type: t, Data: []byte(data)} }

func TestResumeAdmission(t *testing.T) {
	started := ev(engine.EventRunStarted, `{}`)
	cases := []struct {
		name      string
		events    []state.Event
		force     bool
		admit     bool
		label     string
		msgSubstr string // checked only when !admit
	}{
		{"interrupted-noforce", []state.Event{started}, false, true, "", ""},
		{"interrupted-force", []state.Event{started}, true, true, "", ""},
		{"finished-ok-noforce", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"ok"}`)}, false, false, "", "already finished"},
		{"finished-ok-force", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"ok"}`)}, true, false, "", "already finished"},
		{"permanent-noforce", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"permanent_failure"}`)}, false, false, "", "--force"},
		{"permanent-force", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"permanent_failure"}`)}, true, true, "permanent_failure", ""},
		{"rejected-force", []state.Event{started, ev(engine.EventRunFinished, `{"outcome":"rejected"}`)}, true, true, "rejected", ""},
		{"cancelled-noforce", []state.Event{started, ev(engine.EventRunCancelled, `{}`)}, false, false, "", "cancelled"},
		{"cancelled-force", []state.Event{started, ev(engine.EventRunCancelled, `{}`)}, true, true, "cancelled", ""},
		{"crashwindow-permanent-force", []state.Event{started, ev(engine.EventNodeFailed, `{"outcome":"permanent_failure"}`)}, true, true, "permanent_failure", ""},
		{"crashwindow-permanent-noforce", []state.Event{started, ev(engine.EventNodeFailed, `{"outcome":"permanent_failure"}`)}, false, false, "", "--force"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admit, msg, label := resumeAdmission("run-x", tc.events, tc.force)
			if admit != tc.admit {
				t.Fatalf("admit = %v, want %v (msg=%q)", admit, tc.admit, msg)
			}
			if admit && label != tc.label {
				t.Fatalf("label = %q, want %q", label, tc.label)
			}
			if !admit && !strings.Contains(msg, tc.msgSubstr) {
				t.Fatalf("msg = %q, want substring %q", msg, tc.msgSubstr)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/ -run TestResumeAdmission -count=1`
Expected: FAIL — `undefined: resumeAdmission`.

- [ ] **Step 3: Implement the helper**

Create `cli/resume_admission.go`:

```go
package cli

import (
	"fmt"

	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// resumeAdmission decides whether a folded run's event log permits (re-)entry by
// awf resume. It governs ONLY the terminal-outcome guard; pin checks (definition
// digest, runtime drift) are enforced separately by the caller and are NOT
// relaxed by force.
//
// Without force: any terminal marker (run.finished of any outcome, run.cancelled,
// node.failed) refuses; an interrupted run (no terminal marker) is admitted —
// this preserves the historical guard exactly.
//
// With force: a run whose terminal rollup is permanent_failure / rejected /
// cancelled is admitted (run.finished.Outcome is the authority when present;
// run.cancelled and the crash-window node.failed outcome otherwise). A finished
// ok run is never admitted. retryable_failure is also admitted under force (a
// harmless superset). label is the terminal outcome string for the caller's
// warning ("" for an interrupted run).
//
// COORDINATION: the resume-retryable-failure scope-b effort relaxes the
// force=false path to also admit retryable_failure (its §5.1 guard). At merge,
// reconcile to one helper: admit retryable_failure (force-independent) ∪
// {permanent_failure, rejected, cancelled} (force-only).
func resumeAdmission(runID string, events []state.Event, force bool) (admit bool, refuseMsg string, label string) {
	var finished *engine.RunFinishedData
	cancelled := false
	for _, e := range events {
		switch e.Type {
		case engine.EventRunFinished:
			d, err := engine.RunFinishedDataFromEvent(e)
			if err != nil {
				return false, fmt.Sprintf("awf resume: run %q has a corrupt run.finished event: %v\n", runID, err), ""
			}
			finished = &d
		case engine.EventRunCancelled:
			cancelled = true
		}
	}

	if !force {
		if finished != nil {
			return false, fmt.Sprintf("awf resume: run %q already finished (run.finished event in log). Cannot resume a completed run.\n", runID), ""
		}
		if cancelled {
			return false, fmt.Sprintf("awf resume: run %q was cancelled (run.cancelled in log). Cannot resume a cancelled run; start a new run id.\n", runID), ""
		}
		for _, e := range events {
			if e.Type == engine.EventNodeFailed {
				return false, fmt.Sprintf("awf resume: run %q terminated on a failed step (node.failed at path %q in log). Re-run with --force to re-enter after fixing the cause.\n", runID, e.Path), ""
			}
		}
		return true, "", ""
	}

	// force: admit a non-ok terminal run.
	if finished != nil {
		switch engine.Outcome(finished.Outcome) {
		case engine.OutcomeOK:
			return false, fmt.Sprintf("awf resume: run %q already finished (ok). Nothing to resume, even with --force.\n", runID), ""
		case engine.OutcomePermanentFailure, engine.OutcomeRejected, engine.OutcomeRetryableFailure:
			return true, "", finished.Outcome
		default:
			return false, fmt.Sprintf("awf resume: run %q has an unrecognized terminal outcome %q; not resumable even with --force.\n", runID, finished.Outcome), ""
		}
	}
	if cancelled {
		return true, "", "cancelled"
	}
	for _, e := range events {
		if e.Type == engine.EventNodeFailed {
			d, err := engine.NodeFailedDataFromEvent(e)
			if err != nil {
				return false, fmt.Sprintf("awf resume: run %q has a corrupt node.failed event: %v\n", runID, err), ""
			}
			switch engine.Outcome(d.Outcome) {
			case engine.OutcomePermanentFailure, engine.OutcomeRejected, engine.OutcomeRetryableFailure:
				return true, "", d.Outcome
			default:
				return false, fmt.Sprintf("awf resume: run %q terminated with an unrecognized failure %q; not resumable even with --force.\n", runID, d.Outcome), ""
			}
		}
	}
	return true, "", "" // interrupted run (no terminal marker)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cli/ -run TestResumeAdmission -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/resume_admission.go cli/resume_admission_test.go
git commit -m "feat(cli): resumeAdmission guard helper (force widens the admitted set)"
```

---

## Task 3: Gate-budget reset (RunOptions.ForceResume → gate ceiling)

**Files:**
- Modify: `engine/interpreter.go` (`RunOptions` struct; `Run`'s `ictx` literal)
- Modify: `engine/interpreter_context.go` (`interpreterContext` struct)
- Modify: `engine/gate.go` (`runGate` wrapper param; `runGateWithContext` ceiling)
- Test: `engine/gate_test.go`

- [ ] **Step 1: Write the failing test**

Add to `engine/gate_test.go` (mirrors `TestRunGateMidResumeStartsAtNextAttempt`):

```go
func TestRunGateForceResumeGrantsFreshBudget(t *testing.T) {
	// A rejected gate (3/3 attempts folded, all rejected). Under forceResume the
	// gate must run a FRESH allotment numbered ABOVE the committed attempts
	// (4,5,6) and, with a now-passing evaluator, PASS — not immediately re-reject.
	until := ir.Expr("{{ evaluate.verified }}")
	g := &ir.Gate{
		Generate:    ir.NodeList{&ir.CodeStep{ID: "gen1", Run: "echo gen", Container: "c0"}},
		Evaluate:    ir.NodeList{&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()}},
		Until:       until,
		MaxAttempts: 3,
	}
	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeOK, outputs: map[string]any{"verified": true, "feedback": "fixed"}},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	rs.RecordGateAttempt("gate[0]", AttemptResult{N: 1, AttemptOutcome: AttemptRejected, Verdict: map[string]any{"verified": false, "feedback": "a"}})
	rs.RecordGateAttempt("gate[0]", AttemptResult{N: 2, AttemptOutcome: AttemptRejected, Verdict: map[string]any{"verified": false, "feedback": "b"}})
	rs.RecordGateAttempt("gate[0]", AttemptResult{N: 3, AttemptOutcome: AttemptRejected, Verdict: map[string]any{"verified": false, "feedback": "c"}})

	// forceResume = true (final arg).
	oc, err := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil, true)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("got (%q, %v), want (ok, nil) — rejected gate should re-run fresh and pass under force", oc, err)
	}
	got := rs.LookupGateAttempts("gate[0]")
	if len(got) != 4 {
		t.Fatalf("attempts len = %d, want 4 (3 folded + 1 fresh that passed)", len(got))
	}
	if got[3].N != 4 || got[3].AttemptOutcome != AttemptPassed {
		t.Fatalf("fresh attempt = {N:%d,%s}, want {N:4,passed}", got[3].N, got[3].AttemptOutcome)
	}
}

func TestRunGateNoForceRejectedStaysRejected(t *testing.T) {
	// Same setup, forceResume = false: the folded budget is spent, so the gate
	// re-rejects immediately (loop never runs).
	g := &ir.Gate{
		Generate:    ir.NodeList{&ir.CodeStep{ID: "gen1", Run: "echo gen", Container: "c0"}},
		Evaluate:    ir.NodeList{&ir.CodeStep{ID: "eval1", Run: "eval", Container: "c0", OutputSchema: schemaForVerdict()}},
		Until:       ir.Expr("{{ evaluate.verified }}"),
		MaxAttempts: 3,
	}
	disp, lg, blobs := newGateRig(t, map[string]scriptedResult{
		"gen1":  {outcome: OutcomeOK},
		"eval1": {outcome: OutcomeOK, outputs: map[string]any{"verified": true, "feedback": "fixed"}},
	})
	wf := &ir.Workflow{ID: "x", Version: 1, Graph: ir.NodeList{g}}
	rs := NewRunState("run-x", "digest", nil)
	for n := 1; n <= 3; n++ {
		rs.RecordGateAttempt("gate[0]", AttemptResult{N: n, AttemptOutcome: AttemptRejected, Verdict: map[string]any{"verified": false}})
	}
	oc, _ := runGate(context.Background(), g, "gate[0]", wf, rs, disp, lg, blobs, &clock.Fake{}, nil, nil, false)
	if oc != OutcomeRejected {
		t.Fatalf("oc = %q, want rejected (no force, budget spent)", oc)
	}
	if got := rs.LookupGateAttempts("gate[0]"); len(got) != 3 {
		t.Fatalf("attempts len = %d, want 3 (no fresh attempt without force)", len(got))
	}
}
```

- [ ] **Step 2: Update existing `runGate(...)` callers + run to verify compile-fail then test-fail**

The `runGate` test wrapper gains a trailing `forceResume bool`. Append `, false` to every existing `runGate(` call in `engine/gate_test.go` (there are 7; the two new tests above already pass the final bool). Then:

Run: `go test ./engine/ -run 'TestRunGateForceResumeGrantsFreshBudget|TestRunGateNoForceRejectedStaysRejected' -count=1`
Expected: FAIL — first a compile error (`runGate` arity / `forceResume` field unknown), then, once stubs exist, the force test fails (gate re-rejects).

- [ ] **Step 3: Add the `ForceResume` option + field + threading**

In `engine/interpreter.go`, add to `RunOptions` (after `LiveFinalizer`):

```go
	// ForceResume, when true, makes the gate executor grant an exhausted (rejected)
	// gate a fresh attempt allotment on resume (engine/gate.go). Set by
	// `awf resume --force`. No other node kind reads it.
	ForceResume bool
```

In `engine/interpreter.go`, in `Run`'s `ictx := interpreterContext{...}` literal, add:

```go
		forceResume:   opts.ForceResume,
```

In `engine/interpreter_context.go`, add to the `interpreterContext` struct (near `liveFinalizer`):

```go
	forceResume bool
```

- [ ] **Step 4: Add the `forceResume` param to the `runGate` wrapper + the ceiling change**

In `engine/gate.go`, the `runGate` wrapper: add a trailing parameter and pass it into the built `interpreterContext`:

```go
func runGate(
	ctx context.Context,
	g *ir.Gate,
	gatePath string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	broker *signal.Broker,
	forceResume bool,
) (Outcome, error) {
	return runGateWithContext(ctx, g, gatePath, interpreterContext{
		wf: wf, runstate: runstate, dispatcher: dispatcher, log: log, blobs: blobs, clk: clk, tap: tap, broker: broker, forceResume: forceResume,
	})
}
```

In `runGateWithContext`, replace the `startN` + loop header:

```go
	startN := len(ictx.runstate.LookupGateAttempts(gatePath)) + 1
	folded := startN - 1
	ceiling := g.MaxAttempts
	if ictx.forceResume && folded >= g.MaxAttempts {
		// resume --force: an exhausted gate rejected. Grant ONE fresh MaxAttempts
		// allotment numbered ABOVE the committed attempts so attempt-N sub-node
		// paths are uncommitted and really re-run (rather than replaying the old
		// rejected attempts). The prior verdict auto-feeds as repair feedback.
		ceiling = folded + g.MaxAttempts
	}

	for n := startN; n <= ceiling; n++ {
```

And change the last-attempt rejection check from `g.MaxAttempts` to `ceiling`:

```go
		if n == ceiling {
			return OutcomeRejected, fmt.Errorf("engine.runGate: gate %q rejected after %d attempts", gatePath, n)
		}
```

(The fall-through `return OutcomeRejected` at the bottom of the loop stays as the unreachable post-condition.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./engine/ -run 'TestRunGate' -count=1`
Expected: PASS — both new tests plus every pre-existing `TestRunGate*` (the `g.MaxAttempts`→`ceiling` rename is behavior-preserving for non-force, where `ceiling == g.MaxAttempts`).

- [ ] **Step 6: Commit**

```bash
git add engine/interpreter.go engine/interpreter_context.go engine/gate.go engine/gate_test.go
git commit -m "feat(engine): RunOptions.ForceResume + gate fresh-budget reset under force"
```

---

## Task 4: Wire `--force` into `cliResume`

**Files:**
- Modify: `cli/resume.go` (flag, guard replacement, warning, thread to runAndFinish)
- Modify: `cli/execute.go` (`runAndFinish` gains `forceResume bool`)
- Modify: `cli/run.go` (pass `false`)
- Test: `cli/resume_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cli/resume_test.go`. Mirror an existing resume test's harness (Runner with injected fake backend, a run that finishes, then a resume invocation). The minimal force-specific assertions:

```go
func TestResumeForceRefusedWithoutFlag(t *testing.T) {
	// A run that finished permanent_failure; bare `awf resume` refuses and the
	// message mentions --force.
	rig := newResumeTestRig(t) // existing helper: seeds a permanent_failure run on disk, returns runner+args
	code := rig.runner.cliResume([]string{rig.runID, rig.wfPath, "--state-dir", rig.stateDir}, rig.stdout, rig.stderr)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(rig.stderr.String(), "--force") {
		t.Fatalf("stderr should mention --force; got %q", rig.stderr.String())
	}
}

func TestResumeForceAdmitsPermanentFailure(t *testing.T) {
	rig := newResumeTestRig(t) // permanent_failure run; the failing step is reprogrammed to succeed on resume
	rig.reprogramFailedStepToPass()
	code := rig.runner.cliResume([]string{rig.runID, rig.wfPath, "--state-dir", rig.stateDir, "--force"}, rig.stdout, rig.stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr=%q)", code, ExitOK, rig.stderr.String())
	}
	if !strings.Contains(rig.stderr.String(), "force") {
		t.Fatalf("expected a force warning on stderr; got %q", rig.stderr.String())
	}
}
```

> If `cli/resume_test.go` has no reusable rig that seeds a *terminally-failed* run, build a small one in the test file: run a one-step workflow whose code step maps to `permanent_failure` (a non-retryable exit) via the injected fake backend (`container.NewFake()` + `ProgramExec`), assert `cliRun` exits 1 and a `run.finished{permanent_failure}` is in the log, then reprogram the fake's exec for that command to `ExitCode:0` before the `--force` resume. Reuse the existing `Runner` construction pattern already present in `cli/resume_test.go` / `cli/run_test.go`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/ -run 'TestResumeForce' -count=1`
Expected: FAIL — `--force` is an unknown flag (parse error) / the run is refused.

- [ ] **Step 3: Add the `--force` flag**

In `cli/resume.go` `cliResume`, in the flag block (after `stateDir`):

```go
	force := fs0.Bool("force", false, "re-enter a terminally-failed run (permanent_failure/rejected/cancelled); pins are still enforced")
```

Update `printResumeUsage` to add the flag and the usage line `[--force]`:

```go
	fprintln(w, "usage: awf resume <run-id> <path> [--state-dir <dir>] [--force]")
```
and a line:
```go
	fprintln(w, "  --force            re-enter a terminally-failed run (permanent_failure/rejected/cancelled);")
	fprintln(w, "                     replays committed work, re-runs the uncommitted frontier. Pins still enforced.")
```

- [ ] **Step 4: Replace the three terminal-refusal loops with `resumeAdmission`**

In `cli/resume.go`, replace the block at the current Step-3 comment (`// Step 3: terminal-event refusals ...` through the end of the `node.failed` loop) with:

```go
	// Step 3: terminal-outcome guard. resumeAdmission relaxes ONLY the terminal
	// guard under --force; the pin checks below (digest, runtime drift) are
	// unconditional. COORDINATION: reconcile with the retryable scope-b guard at
	// merge (one helper; admit retryable always ∪ {permanent,rejected,cancelled}
	// under --force).
	admit, refuseMsg, termLabel := resumeAdmission(runID, events, *force)
	if !admit {
		fprintf(stderr, "%s", refuseMsg)
		return ExitUsage
	}
	if *force && termLabel != "" {
		fprintf(stderr, "awf resume --force: re-entering a terminally-%s run %q; the uncommitted frontier (and its side effects) will re-run. Pins remain enforced.\n", termLabel, runID)
	}
```

- [ ] **Step 5: Thread force → runAndFinish → RunOptions.ForceResume**

In `cli/execute.go`, add a `forceResume bool` parameter to `runAndFinish` (after `skipTeardown *bool`, or grouped with the other scalars) and set it on the options:

```go
	outcome, runErr := engine.Run(ctx, ld, rs, dispatcher, log, blobs, clock.System{}, engine.RunOptions{
		Tap:           stdout,
		Broker:        broker,
		Assets:        assets,
		InputFiles:    inputFiles,
		LiveFinalizer: liveDispatchFinalizer(liveRoot),
		ForceResume:   forceResume,
	})
```

In `cli/resume.go`, pass `*force` at the `runAndFinish` call (last arg):

```go
	return r.runAndFinish(ctx, backend, resolverOrEmpty(resolver), ld, rs, handles, log, blobs, stdout, stderr, runID, "awf resume", " (resumed)", recordedAssets, nil, broker, liveRoot, &skipTeardown, *force)
```

In `cli/run.go` (the only other caller), pass `false`:

```go
	return r.runAndFinish(ctx, backend, resolverOrEmpty(resolver), ld, rs, handles, log, blobs, stdout, stderr, id, "awf run", "", assetSnapshots, inputFileRefs, broker, liveRoot, &skipTeardown, false)
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./cli/ -run 'TestResumeForce|TestResumeAdmission' -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cli/resume.go cli/execute.go cli/run.go cli/resume_test.go
git commit -m "feat(cli): awf resume --force flag, guard, warning, ForceResume threading"
```

---

## Task 5: Conformance — frontier re-run, cancelled, pins-enforced

**Files:**
- Test: `cli/resume_test.go` (or `engine/conformance_*_test.go` if that is where end-to-end resume conformance lives — match the existing convention)

- [ ] **Step 1: Write the failing/■ tests**

Three fake-backend tests (mirror the harness from Task 4):

```go
// Pins are STILL enforced under --force: a changed definition digest aborts.
func TestResumeForceStillEnforcesDigestPin(t *testing.T) {
	rig := newResumeTestRig(t) // permanent_failure run
	rig.mutateWorkflowFile()   // edit the on-disk wf so its digest no longer matches
	code := rig.runner.cliResume([]string{rig.runID, rig.wfPath, "--state-dir", rig.stateDir, "--force"}, rig.stdout, rig.stderr)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (digest pin must hold under --force)", code, ExitUsage)
	}
	if !strings.Contains(rig.stderr.String(), "digest mismatch") {
		t.Fatalf("want digest-mismatch error; got %q", rig.stderr.String())
	}
}

// A cancelled run re-enters under --force and its frontier completes.
func TestResumeForceAdmitsCancelled(t *testing.T) {
	rig := newCancelledResumeTestRig(t) // a run with run.cancelled in the log + an uncommitted frontier step
	code := rig.runner.cliResume([]string{rig.runID, rig.wfPath, "--state-dir", rig.stateDir, "--force"}, rig.stdout, rig.stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (force admits cancelled; stderr=%q)", code, ExitOK, rig.stderr.String())
	}
}
```

For the digest-pin test, `mutateWorkflowFile` appends a semantically-irrelevant change that alters the digest (e.g. a new container alias) — the point is `rs.WorkflowDigest != currentDigest` fires (cli/resume.go:222) *before* execution, regardless of `--force`.

> If a `newCancelledResumeTestRig` does not exist, construct it: run a two-step workflow, deliver a cancel via the broker so the run writes `run.cancelled` with step 2 uncommitted (mirror an existing cancel test in `cli/`), then `--force` resume and assert step 2 runs (`ProgramExec` for step-2's command is hit) and the run finishes ok.

- [ ] **Step 2: Run to verify**

Run: `go test ./cli/ -run 'TestResumeForce' -count=1`
Expected: the digest-pin test PASSES immediately (no code change needed — pins are already unconditional, this is a guard-against-regression); the cancelled test PASSES (admission already implemented in Task 4; frontier re-run is free).

- [ ] **Step 3: Verify the `live_resume_preflight` cancelled path (spec §17)**

Confirm `preflightLiveResume` / `engine.LiveResumePreflightRequests` does not block a forced cancelled re-entry for a workflow with NO persistent-session live agents (the common case): the cancelled test above uses code steps only, so a green result confirms it. If a future persistent-session workflow needs gating, that is tracked separately (do not add it here unless the test forces it).

Run: `go test ./cli/ ./engine/ -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cli/resume_test.go
git commit -m "test(cli): resume --force conformance — frontier re-run, cancelled, pins enforced"
```

---

## Task 6: Docs

**Files:**
- Modify: `man/awf.1.md` (resume section)

- [ ] **Step 1: Update the resume man page**

In `man/awf.1.md`, update the `awf resume` synopsis to `awf resume _run-id_ _path_ [--force]` and add, after the `--state-dir` entry:

```
**--force**
:   Re-enter a run that ended terminally — `permanent_failure`, `rejected`, or
    `cancelled`. Committed steps are still replayed; the uncommitted frontier
    re-runs (a rejected gate re-runs from a fresh attempt budget). **Pinning is
    not relaxed:** a changed definition digest or runtime version is still a hard
    error — `--force` only overrides the terminal-outcome refusal. Because the
    frontier re-runs, its side effects can repeat (at-least-once); use `--force`
    only after fixing the deterministic cause of the failure.
```

Also update the prose that says a finished/failed run "cannot be resumed" to note the `--force` exception.

- [ ] **Step 2: Verify the manual builds / lints (if a docs check exists), else visual check**

Run: `git diff man/awf.1.md` and confirm formatting matches the surrounding entries.

- [ ] **Step 3: Commit**

```bash
git add man/awf.1.md
git commit -m "docs(man): document awf resume --force"
```

---

## Final verification

- [ ] **Run the full gate** (matches CI; the project verification bar is `make lint test`, NOT `go test`/`vet`/`gofmt` alone):

Run: `make lint test`
Expected: clean (0 lint issues, all tests pass). Note: a pre-existing unrelated `.claude/worktrees/drift-fixes` gofmt issue is outside this worktree and not our concern; if `make lint` aborts on it, run `golangci-lint run ./engine/... ./cli/...` and `go test ./...` directly to confirm our packages are clean.

- [ ] **Self-check against the spec** (§5–§14): guard admits the three outcomes under force + refuses ok (Task 2/4); pins enforced under force (Task 5); frontier re-run free (Task 5 cancelled + Task 4 permanent); gate budget reset (Task 3); deferred permanent map-item documented (no task — out of scope); docs updated (Task 6).

---

## Coordination summary (for the merge with retryable scope-b)

1. **Accessors** (Task 1) — keep one copy of `RunFinishedDataFromEvent` / `NodeFailedDataFromEvent`.
2. **Guard** (Task 2/4) — `resumeAdmission(force=false)` preserves current behavior (refuses terminal incl. retryable). The retryable effort relaxes force=false to admit `retryable_failure`. Reconcile to one helper: admit `retryable_failure` ∪ (`--force` ? `{permanent_failure, rejected, cancelled}` : ∅). **Latest-wins:** the merged helper must read the LAST `run.finished` (this plan's helper does NOT `break` on the first), because a force-resumed run appends a *second* `run.finished` — the retryable §5.1 draft's `break`-on-first would read a stale terminal outcome after a force-resume.
3. **RunOptions** — the retryable effort adds `Resume bool`; this plan adds `ForceResume bool`. Decide at merge whether they fold into one resume-mode (spec §17).
