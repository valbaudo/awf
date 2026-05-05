package engine_test

import (
	"context"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

func TestNodeStartedEmittedOncePerExecutedStep(t *testing.T) {
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./s.sh", container.ExecResult{ExitCode: 0, Stdout: []byte("ok\n")}, nil)
	wf := &ir.Workflow{Graph: ir.NodeList{&ir.CodeStep{ID: "s1", Container: "lab", Run: "./s.sh"}}}
	def := &ir.LoadedDefinition{Workflow: wf}

	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if starts, completes := countNodeEvents(t, log); starts != 1 || completes != 1 {
		t.Fatalf("got starts=%d completes=%d, want 1/1", starts, completes)
	}

	// Resume: fold the same log into a fresh RunState, re-run. The committed
	// node short-circuits (replayed, not recomputed) → NO second node.started.
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	rs2, err := engine.Fold(events, blobs)
	if err != nil {
		t.Fatalf("engine.Fold: %v", err)
	}
	if _, err := engine.Run(context.Background(), def, rs2, disp, log, blobs, clk, nil, nil); err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if starts, _ := countNodeEvents(t, log); starts != 1 {
		t.Fatalf("after resume got %d node.started, want 1 (replay must not re-emit)", starts)
	}
}

func countNodeEvents(t *testing.T, log *state.InMemoryLog) (starts, completes int) {
	t.Helper()
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	for _, e := range events {
		switch e.Type {
		case engine.EventNodeStarted:
			starts++
		case engine.EventNodeCompleted:
			completes++
		}
	}
	return starts, completes
}
