package engine_test

import (
	"bytes"
	"context"
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
