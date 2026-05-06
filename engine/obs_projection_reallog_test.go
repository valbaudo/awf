package engine_test

import (
	"context"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/obs"
)

// intPtr is a local helper — engine/try_test.go defines the same function but
// in package engine (internal), so it is not visible here in engine_test.
func intPtr(n int) *int { return &n }

// TestObsProjectMatchesAddressingTreeRealLog is the 6.1 exit-criterion test.
//
// It drives a real engine.Run over a two-node workflow (a code step `a`
// followed by a 2-iteration loop whose body contains code step `b`), folds the
// resulting log, and asserts that obs.Project produces EXACTLY the addressing
// tree — every node path + every ParentPath ancestor + the run root "".
//
// A mismatch is a genuine bug: either obs synthesizes a scope the engine never
// addressed (orphan) or it misses one — the fabricated-event unit tests
// (Tasks 9–15) cannot catch that divergence; only a real log can.
func TestObsProjectMatchesAddressingTreeRealLog(t *testing.T) {
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./a.sh", container.ExecResult{ExitCode: 0}, nil)
	// Two iterations → two execs of body.sh.
	fake.ProgramExec("./body.sh", container.ExecResult{ExitCode: 0}, nil)
	fake.ProgramExec("./body.sh", container.ExecResult{ExitCode: 0}, nil)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "a", Container: "lab", Run: "./a.sh"},
		&ir.Loop{MaxIters: intPtr(2), Body: ir.NodeList{
			&ir.CodeStep{ID: "b", Container: "lab", Run: "./body.sh"},
		}},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}
	if _, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}

	// Expected = every real node path + all its ParentPath ancestors + root "".
	// Built with the SAME ParentPath obs uses, so an exact match proves obs
	// synthesizes precisely the addressing tree — no orphans, no missing scopes.
	want := map[string]bool{"": true}
	for _, e := range events {
		// R4: every node-bearing event creates a span in Project, so include
		// failed/skipped paths too — otherwise the test is silently coupled to an
		// all-ok fixture and a future failing/skipped step makes got ⊋ want.
		switch e.Type {
		case engine.EventNodeStarted, engine.EventNodeCompleted, engine.EventNodeFailed, engine.EventNodeSkipped:
			for p := e.Path; ; {
				want[p] = true
				parent, ok := engine.ParentPath(p)
				if !ok {
					break
				}
				p = parent
			}
		}
	}

	spans, err := obs.Project(events, nil)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	got := map[string]bool{}
	for _, s := range spans {
		got[s.Path] = true
	}
	for p := range want {
		if !got[p] {
			t.Errorf("addressing path %q missing from projected spans", p)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("projected span %q is not in the addressing tree", p)
		}
	}
}
