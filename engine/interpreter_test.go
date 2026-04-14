package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// refExpr returns a *ir.Expr pointer to the given string — slim sugar for IR
// construction in tests (ir.Loop.Until is *ir.Expr, omitempty).
func refExpr(s string) *ir.Expr {
	e := ir.Expr(s)
	return &e
}

// newRunHarness builds the in-mem fakes + a default RunState seeded with
// run.started + a single-container handle map.
func newRunHarness(t *testing.T) (*container.Fake, container.Handle, *engine.LocalDispatcher, *state.InMemoryLog, *state.InMemoryBlobs, *clock.Fake, *engine.RunState) {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	disp := &engine.LocalDispatcher{
		Backend: fake,
		Handles: map[string]container.Handle{"lab": h},
	}
	log := state.NewInMemoryLog(clk)
	blobs := state.NewInMemoryBlobs()
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: []byte(`{"run_id":"r1","workflow_digest":"d1"}`)}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
	rs := engine.NewRunState("r1", "d1", nil)
	return fake, h, disp, log, blobs, clk, rs
}

func TestRunEmptyGraphIsOK(t *testing.T) {
	t.Parallel()
	_, _, disp, log, blobs, clk, rs := newRunHarness(t)
	wf := &ir.Workflow{Graph: ir.NodeList{}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", oc)
	}
	events, _ := log.Fold()
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted {
			t.Errorf("unexpected node.completed event on empty graph: %+v", e)
		}
	}
}

func TestRunSingleCodeStepHappyPath(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./hello.sh", container.ExecResult{
		ExitCode: 0,
		Stdout:   []byte("hello\n"),
	}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "hello", Container: "lab", Run: "./hello.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	var tap bytes.Buffer
	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, &tap)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", oc)
	}
	nr, ok := rs.Completed["hello"]
	if !ok {
		t.Fatal("RunState.Completed missing 'hello'")
	}
	if nr.Outcome != engine.OutcomeOK || string(nr.Stdout) != "hello\n" {
		t.Errorf("nr = %+v, want ok + stdout 'hello'", nr)
	}
	events, _ := log.Fold()
	var sawCompleted bool
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted && e.Path == "hello" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Errorf("no node.completed event for 'hello'; events: %+v", events)
	}
}

func TestRunSequentialCodeStepsResolveCrossStepRefs(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./step1.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"greeting":"hello"}`),
		Stdout:    []byte("step1\n"),
	}, nil)
	fake.ProgramExec("./step2.sh hello", container.ExecResult{
		ExitCode: 0,
		Stdout:   []byte("step2\n"),
	}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{
			ID: "step1", Container: "lab", Run: "./step1.sh",
			OutputSchema: &ir.JSONSchema{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []any{"greeting"},
				"properties":           map[string]any{"greeting": map[string]any{"type": "string"}},
			},
		},
		&ir.CodeStep{ID: "step2", Container: "lab", Run: "./step2.sh {{ step.step1.greeting }}"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", oc)
	}
	if _, ok := rs.Completed["step1"]; !ok {
		t.Error("RunState.Completed missing step1")
	}
	if _, ok := rs.Completed["step2"]; !ok {
		t.Error("RunState.Completed missing step2")
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("fake.Calls len = %d, want 2", len(fake.Calls))
	}
	if fake.Calls[1].Run != "./step2.sh hello" {
		t.Errorf("step2 command = %q, want substituted %q", fake.Calls[1].Run, "./step2.sh hello")
	}
}

func TestRunCodeStepIdempotencyKeySubstituted(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	rs.Input = map[string]any{"cve_id": "CVE-2024-9999"}
	fake.ProgramExec("./open-pr.sh", container.ExecResult{ExitCode: 0}, nil)

	idemTpl := ir.Template("{{ input.cve_id }}:pr")
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{
			ID:             "open_pr",
			Container:      "lab",
			Run:            "./open-pr.sh",
			IdempotencyKey: &idemTpl,
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := fake.Calls[0].Env["AWF_IDEMPOTENCY_KEY"]
	if got != "CVE-2024-9999:pr" {
		t.Errorf("AWF_IDEMPOTENCY_KEY = %q, want %q", got, "CVE-2024-9999:pr")
	}
}

func TestRunCodeStepFailureAppendsNodeFailed(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./misconfig.sh", container.ExecResult{ExitCode: 78}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "misconfig", Container: "lab", Run: "./misconfig.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if oc != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure", oc)
	}
	if err == nil {
		t.Error("err is nil; want underlying failure cause")
	}
	if _, ok := rs.Completed["misconfig"]; ok {
		t.Error("RunState.Completed has 'misconfig' — failed steps must NOT commit")
	}
	events, _ := log.Fold()
	var failedFound bool
	for _, e := range events {
		if e.Type == engine.EventNodeFailed && e.Path == "misconfig" {
			failedFound = true
		}
	}
	if !failedFound {
		t.Errorf("no node.failed event for 'misconfig'; events: %+v", events)
	}
}

func TestRunCodeStepFailureHaltsSubsequentSteps(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./step1.sh", container.ExecResult{ExitCode: 78}, nil)
	fake.ProgramExec("./step2.sh", container.ExecResult{ExitCode: 0}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "step1", Container: "lab", Run: "./step1.sh"},
		&ir.CodeStep{ID: "step2", Container: "lab", Run: "./step2.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	_, _ = engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if len(fake.Calls) != 1 {
		t.Errorf("fake.Calls len = %d, want 1 (step2 must NOT dispatch after step1 fails)", len(fake.Calls))
	}
	if fake.Calls[0].Run != "./step1.sh" {
		t.Errorf("fake.Calls[0].Run = %q, want %q", fake.Calls[0].Run, "./step1.sh")
	}
}

func TestRunCodeStepTemplateErrorIsPermanent(t *testing.T) {
	t.Parallel()
	_, _, disp, log, blobs, clk, rs := newRunHarness(t)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "bad", Container: "lab", Run: "./run.sh {{ step.nonexistent.field }}"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if oc != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure (template error is author bug, slice 2.5 DQ7)", oc)
	}
	if err == nil {
		t.Fatal("err is nil; want AWF4002")
	}
	if !strings.Contains(err.Error(), "AWF4002") {
		t.Errorf("err = %v, want mention of AWF4002", err)
	}
	events, _ := log.Fold()
	var found bool
	for _, e := range events {
		if e.Type == engine.EventNodeFailed && e.Path == "bad" {
			found = true
		}
	}
	if !found {
		t.Errorf("no node.failed event; events: %+v", events)
	}
}

func TestRunCodeStepLiveTapWritesStepIDPrefixedChunks(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./chunky.sh", container.ExecResult{
		ExitCode: 0,
		Stdout:   []byte("done\n"),
	}, []container.IOChunk{
		{Stream: "stdout", Data: []byte("line 1\n")},
		{Stream: "stdout", Data: []byte("line 2\n")},
		{Stream: "stderr", Data: []byte("warn: thing happened\n")},
	})

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "chunky", Container: "lab", Run: "./chunky.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	var tap bytes.Buffer
	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, &tap); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := tap.String()
	wantLines := []string{
		"[chunky] line 1\n",
		"[chunky] line 2\n",
		"[chunky] warn: thing happened\n",
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("tap missing %q; got %q", w, out)
		}
	}
}

func TestRunSkipsAlreadyCompletedNodes(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	exit := 0
	rs.Completed["already_done"] = engine.NodeResult{
		Outcome:  engine.OutcomeOK,
		ExitCode: &exit,
		Stdout:   []byte("previously committed\n"),
	}
	fake.ProgramExec("./would-fail.sh", container.ExecResult{ExitCode: 1}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "already_done", Container: "lab", Run: "./would-fail.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok", oc)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("fake.Calls len = %d, want 0 (committed steps must NOT re-execute)", len(fake.Calls))
	}
}

func TestRunPhase2UnsupportedKindsAllErrorWithSentinel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		node ir.Node
	}{
		{"agent", &ir.AgentStep{ID: "ag", Container: "lab", Uses: "anthropic/claude-code"}},
		{"signal", &ir.SignalStep{ID: "sig", Await: "human_review"}},
		{"try", &ir.Try{Do: ir.NodeList{&ir.CodeStep{ID: "x", Container: "lab", Run: "./x.sh"}}}},
		{"parallel", &ir.Parallel{Children: ir.NodeList{&ir.CodeStep{ID: "x", Container: "lab", Run: "./x.sh"}}}},
		{"gate", &ir.Gate{
			Generate:    ir.NodeList{&ir.CodeStep{ID: "g", Container: "lab", Run: "./g.sh"}},
			Evaluate:    ir.NodeList{&ir.CodeStep{ID: "e", Container: "lab", Run: "./e.sh"}},
			Until:       ir.Expr("true"),
			MaxAttempts: 1,
		}},
		{"map", &ir.Map{
			Over: ir.Expr("input.items"), As: "item", Container: "lab", Concurrency: 1,
			Body: ir.NodeList{&ir.CodeStep{ID: "x", Container: "lab", Run: "./x.sh"}},
		}},
		{"skip", &ir.Skip{Reason: "phase-2-test"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, disp, log, blobs, clk, rs := newRunHarness(t)
			def := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{c.node}}}
			_, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
			if !errors.Is(err, engine.ErrNodeNotImplementedInPhase2) {
				t.Errorf("err = %v, want errors.Is(_, ErrNodeNotImplementedInPhase2)", err)
			}
		})
	}
}

func TestRunCodeStepRetryableExhaustionAppendsNodeFailed(t *testing.T) {
	// After retry exhaustion, the interpreter's runCodeStep MUST route through
	// failStep with outcome=retryable_failure — the node.failed event records
	// the LAST attempt's error (matches RunWithRetry's "exhausted-as-failure"
	// contract). Distinct from the permanent_failure path (exit code in
	// NonRetryableExitCodes), which is already covered by
	// TestRunCodeStepFailureAppendsNodeFailed.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	// Exit 1 is a generic nonzero (retryable). With retry.Default (3 attempts),
	// all 3 attempts fail; RunWithRetry returns the last attempt's error.
	fake.ProgramExec("./flaky.sh", container.ExecResult{
		ExitCode: 1,
		Stdout:   []byte("transient failure\n"),
	}, nil)

	// Override the retry policy so attempts run instantly (no real sleeps);
	// the fake clock advances synthetically via clock.Fake.Sleep, but we
	// still pay the overhead of the 3-attempt loop. Use a CodeStep with an
	// explicit no-backoff override so the test is fast.
	noBackoff := "none"
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{
			ID: "flaky", Container: "lab", Run: "./flaky.sh",
			Retry: &ir.RetryPolicy{Attempts: 2, Backoff: noBackoff},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if oc != engine.OutcomeRetryableFailure {
		t.Errorf("Outcome = %v, want retryable_failure", oc)
	}
	if err == nil {
		t.Fatal("err = nil; want last-attempt error")
	}
	// No node.completed for the failed step.
	if _, ok := rs.Completed["flaky"]; ok {
		t.Error("RunState.Completed has 'flaky' — failed steps must NOT commit")
	}
	// node.failed event landed with outcome=retryable_failure.
	events, _ := log.Fold()
	var failedFound bool
	var failedOutcome string
	for _, e := range events {
		if e.Type == engine.EventNodeFailed && e.Path == "flaky" {
			failedFound = true
			var d engine.NodeFailedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal node.failed: %v", err)
			}
			failedOutcome = d.Outcome
		}
	}
	if !failedFound {
		t.Errorf("no node.failed event for 'flaky'; events: %+v", events)
	}
	if failedOutcome != string(engine.OutcomeRetryableFailure) {
		t.Errorf("node.failed outcome = %q, want %q", failedOutcome, engine.OutcomeRetryableFailure)
	}
	// Verify all retry attempts actually ran (2 dispatches for Attempts:2).
	if len(fake.Calls) != 2 {
		t.Errorf("fake.Calls len = %d, want 2 (retry exhaustion)", len(fake.Calls))
	}
}

func TestRunUnknownContainerIsInternalError(t *testing.T) {
	t.Parallel()
	_, _, disp, log, blobs, clk, rs := newRunHarness(t)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "bad", Container: "no_such_container", Run: "./whatever.sh"},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if oc != "" {
		t.Errorf("Outcome = %q, want empty (internal error, not a step outcome)", oc)
	}
	if err == nil {
		t.Fatal("err is nil; want unknown-container error")
	}
}

func TestRunIfThenBranchTaken(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./step_in_then.sh", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("./step_in_else.sh", container.ExecResult{ExitCode: 0}, nil)
	rs.Input = map[string]any{"do_it": true}

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.do_it }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "in_then", Container: "lab", Run: "./step_in_then.sh"},
			},
			Else: ir.NodeList{
				&ir.CodeStep{ID: "in_else", Container: "lab", Run: "./step_in_else.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: %v / %v", oc, err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(fake.Calls))
	}
	if fake.Calls[0].Run != "./step_in_then.sh" {
		t.Errorf("ran %q, want ./step_in_then.sh", fake.Calls[0].Run)
	}
	events, _ := log.Fold()
	var bt *engine.BranchTakenData
	var btPath string
	for _, e := range events {
		if e.Type == engine.EventBranchTaken {
			var d engine.BranchTakenData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal branch.taken: %v", err)
			}
			bt = &d
			btPath = e.Path
		}
	}
	if bt == nil {
		t.Fatal("no branch.taken event in log")
	}
	if bt.Which != "then" || btPath != "if[0]" {
		t.Errorf("branch.taken = %+v at path %q, want {Which:then} at if[0]", bt, btPath)
	}
	if rs.Branches["if[0]"] != "then" {
		t.Errorf("rs.Branches[if[0]] = %q, want %q", rs.Branches["if[0]"], "then")
	}
}

func TestRunIfElseBranchTaken(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./step_in_else.sh", container.ExecResult{ExitCode: 0}, nil)
	rs.Input = map[string]any{"do_it": false}

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.do_it }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "in_then", Container: "lab", Run: "./step_in_then.sh"},
			},
			Else: ir.NodeList{
				&ir.CodeStep{ID: "in_else", Container: "lab", Run: "./step_in_else.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	_, _ = engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if len(fake.Calls) != 1 || fake.Calls[0].Run != "./step_in_else.sh" {
		t.Errorf("dispatched %+v, want only ./step_in_else.sh", fake.Calls)
	}
	if rs.Branches["if[0]"] != "else" {
		t.Errorf("rs.Branches[if[0]] = %q, want %q", rs.Branches["if[0]"], "else")
	}
}

func TestRunIfNoElseFalseCondIsNoOp(t *testing.T) {
	// Spec §5.1: "A false cond with no else is a no-op." Branch.taken still
	// fires (Which:"else") so resume knows the decision was made.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	rs.Input = map[string]any{"do_it": false}

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.do_it }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "in_then", Container: "lab", Run: "./never.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: %v / %v", oc, err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("dispatched %d cmds, want 0", len(fake.Calls))
	}
	if rs.Branches["if[0]"] != "else" {
		t.Errorf("rs.Branches[if[0]] = %q, want %q", rs.Branches["if[0]"], "else")
	}
	// Verify the branch.taken event landed in the log (not just in the in-mem
	// map). A divergence between rs.Branches and the log would mean resume
	// re-evaluates the cond — the test was previously weaker than the spec.
	events, _ := log.Fold()
	var bt *engine.BranchTakenData
	for _, e := range events {
		if e.Type == engine.EventBranchTaken && e.Path == "if[0]" {
			var d engine.BranchTakenData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal branch.taken: %v", err)
			}
			bt = &d
		}
	}
	if bt == nil {
		t.Fatal("no branch.taken event in log")
	}
	if bt.Which != "else" {
		t.Errorf("branch.taken Which = %q, want %q", bt.Which, "else")
	}
}

func TestRunIfResumeSkipsCondEvaluation(t *testing.T) {
	// rs.Branches[if[0]]="then" simulates a resume where the branch decision
	// was already committed. Re-evaluating cond would be wrong — cond depends
	// on inputs/step outputs that may have changed.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./step_in_then.sh", container.ExecResult{ExitCode: 0}, nil)
	rs.Branches["if[0]"] = "then"
	// Input would evaluate to else if re-evaluated.
	rs.Input = map[string]any{"do_it": false}

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.do_it }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "in_then", Container: "lab", Run: "./step_in_then.sh"},
			},
			Else: ir.NodeList{
				&ir.CodeStep{ID: "in_else", Container: "lab", Run: "./step_in_else.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: %v / %v", oc, err)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Run != "./step_in_then.sh" {
		t.Errorf("ran %+v, want ./step_in_then.sh (recorded branch)", fake.Calls)
	}
	events, _ := log.Fold()
	var branchTakenCount int
	for _, e := range events {
		if e.Type == engine.EventBranchTaken {
			branchTakenCount++
		}
	}
	if branchTakenCount != 0 {
		t.Errorf("emitted %d branch.taken events on resume, want 0 (recorded branch)", branchTakenCount)
	}
}

func TestRunIfCondTypeMismatchIsPermanent(t *testing.T) {
	// Spec §7: bounded evaluator, no coercion. Non-bool top-level cond is
	// AWF4003. Per DQ7, route as permanent_failure for the if NODE.
	t.Parallel()
	_, _, disp, log, blobs, clk, rs := newRunHarness(t)
	rs.Input = map[string]any{"count": 5} // int, not bool

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: ir.Expr("{{ input.count }}"),
			Then: ir.NodeList{
				&ir.CodeStep{ID: "in_then", Container: "lab", Run: "./never.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if oc != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure", oc)
	}
	if err == nil || !strings.Contains(err.Error(), "AWF4003") {
		t.Errorf("err = %v, want AWF4003", err)
	}
	events, _ := log.Fold()
	var found bool
	for _, e := range events {
		if e.Type == engine.EventNodeFailed && e.Path == "if[0]" {
			found = true
		}
	}
	if !found {
		t.Errorf("no node.failed event for if[0]; events: %+v", events)
	}
}

func TestRunLoopWithMaxItersOnly(t *testing.T) {
	// 3-iter loop, no until. Each iter runs the body once; loop.iter fires
	// once per completed iter; LoopIters[path] ends at 3.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{ExitCode: 0}, nil)
	max := 3
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{ID: "body_step", Container: "lab", Run: "./body.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: %v / %v", oc, err)
	}
	if rs.LoopIters["loop[0]"] != 3 {
		t.Errorf("rs.LoopIters[loop[0]] = %d, want 3", rs.LoopIters["loop[0]"])
	}
	if len(fake.Calls) != 3 {
		t.Errorf("body dispatched %d times, want 3", len(fake.Calls))
	}
	events, _ := log.Fold()
	var iterEvents int
	for _, e := range events {
		if e.Type == engine.EventLoopIter && e.Path == "loop[0]" {
			iterEvents++
		}
	}
	if iterEvents != 3 {
		t.Errorf("emitted %d loop.iter events, want 3", iterEvents)
	}
}

func TestRunLoopWithUntilOnly(t *testing.T) {
	// Body produces a typed output `done: true`; until reads it → true on
	// iter 1 → loop exits after 1 iter.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"done":true}`),
	}, nil)
	max := 5
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			Until:    refExpr("{{ step.body_step.done }}"),
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{
					ID: "body_step", Container: "lab", Run: "./body.sh",
					OutputSchema: &ir.JSONSchema{
						"type":                 "object",
						"additionalProperties": false,
						"required":             []any{"done"},
						"properties":           map[string]any{"done": map[string]any{"type": "boolean"}},
					},
				},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("Run: %v / %v", oc, err)
	}
	if rs.LoopIters["loop[0]"] != 1 {
		t.Errorf("rs.LoopIters[loop[0]] = %d, want 1 (until true on iter 1)", rs.LoopIters["loop[0]"])
	}
	if len(fake.Calls) != 1 {
		t.Errorf("body dispatched %d times, want 1", len(fake.Calls))
	}
}

func TestRunLoopBodyFailureDoesNotEmitLoopIter(t *testing.T) {
	// DQ8: if body[K] fails, loop.iter{K} is NOT emitted.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./flaky.sh", container.ExecResult{ExitCode: 78}, nil)
	max := 3
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{ID: "flaky", Container: "lab", Run: "./flaky.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, _ := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if oc != engine.OutcomePermanentFailure {
		t.Errorf("Outcome = %v, want permanent_failure", oc)
	}
	if rs.LoopIters["loop[0]"] != 0 {
		t.Errorf("rs.LoopIters[loop[0]] = %d, want 0 (iter 1 failed mid-flight, never committed)", rs.LoopIters["loop[0]"])
	}
	events, _ := log.Fold()
	var iterEvents int
	for _, e := range events {
		if e.Type == engine.EventLoopIter {
			iterEvents++
		}
	}
	if iterEvents != 0 {
		t.Errorf("emitted %d loop.iter events, want 0 (body failed before iter completed)", iterEvents)
	}
}

func TestRunLoopResumeContinuesFromLastCompletedIter(t *testing.T) {
	// Pre-populate rs.LoopIters[loop[0]] = 2 — simulates resume where iters
	// 1 and 2 committed and iter 3 was in-flight.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{ExitCode: 0}, nil)
	rs.LoopIters["loop[0]"] = 2

	max := 4
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{ID: "body_step", Container: "lab", Run: "./body.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.Calls) != 2 {
		t.Errorf("body dispatched %d times, want 2 (resume from iter 3 to max=4)", len(fake.Calls))
	}
	if rs.LoopIters["loop[0]"] != 4 {
		t.Errorf("rs.LoopIters[loop[0]] = %d, want 4", rs.LoopIters["loop[0]"])
	}
}

func TestRunLoopBodyStepPathIncludesIterSuffix(t *testing.T) {
	// node.completed events must use path "loop[0].body.iter-K.body_step" —
	// the addressing grammar from slice 2.1.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{ExitCode: 0}, nil)
	max := 2
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			MaxIters: &max,
			Body: ir.NodeList{
				&ir.CodeStep{ID: "body_step", Container: "lab", Run: "./body.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, _ := log.Fold()
	wantPaths := map[string]bool{
		"loop[0].body.iter-1.body_step": false,
		"loop[0].body.iter-2.body_step": false,
	}
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted {
			if _, want := wantPaths[e.Path]; want {
				wantPaths[e.Path] = true
			}
		}
	}
	for path, found := range wantPaths {
		if !found {
			t.Errorf("no node.completed event at path %q", path)
		}
	}
}

func TestRunLoopNeitherUntilNorMaxIsInternalError(t *testing.T) {
	// Slice 2.5 R7: validator (ir/validate_structural.go:86) enforces
	// "at least one of until / max_iters" (AWF §5.2). If validation
	// regresses, the runtime must fail LOUD — silently exiting on iter 1
	// would mask the bug.
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./body.sh", container.ExecResult{ExitCode: 0}, nil)

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			// Until nil, MaxIters nil — would be rejected by validator,
			// but the runtime defends.
			Body: ir.NodeList{
				&ir.CodeStep{ID: "body_step", Container: "lab", Run: "./body.sh"},
			},
		},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil)
	if oc != "" {
		t.Errorf("Outcome = %q, want empty (internal error)", oc)
	}
	if err == nil {
		t.Fatal("err is nil; want validator-regression error")
	}
	if !strings.Contains(err.Error(), "validator regression") {
		t.Errorf("err = %v, want mention of 'validator regression'", err)
	}
}
