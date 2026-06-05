package engine_test

import (
	"context"
	"testing"

	"github.com/valbaudo/awf/agent"
	agentfake "github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// TestRunAgent_EmptyContainer_PassesZeroHandle verifies that an AgentStep with
// an empty Container field does not trigger a "no handle" error. The dispatcher
// must pass container.Handle{} to the adapter when the container ref is empty;
// a Containerless fake adapter ignores the handle and returns typed output.
func TestRunAgent_EmptyContainer_PassesZeroHandle(t *testing.T) {
	ctx := context.Background()

	fk := agentfake.New("awf/llm").WithCaps(agent.Caps{Containerless: true}).
		Script(0, agentfake.Result{Output: map[string]any{"answer": "42"}})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// No handles — containerless steps must not consult d.Handles at all.
	d := &engine.LocalDispatcher{
		Resolver: &reg,
		Handles:  map[string]container.Handle{},
	}
	intent := engine.NodeIntent{
		Path:           "graph[0]",
		Node:           &ir.AgentStep{ID: "ask", Uses: "awf/llm", Container: ""},
		ResolvedInputs: engine.ResolvedInputs{Uses: "awf/llm", With: ir.RawConfig{"model": "m", "prompt": "hi"}},
	}

	dr, ch, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	for range ch {
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q (Err: %v)", dr.Outcome, engine.OutcomeOK, dr.Err)
	}
}

// TestDispatcherCapturesSnapshotOnEligibleAgentStep is the agent-step counterpart
// to TestDispatcherCapturesSnapshotOnEligibleOkStep (in local_dispatcher_test.go):
// it exercises runAgent's snapshot:workspace capture block. An eligible agent step
// that succeeds (OutcomeOK) must capture the CoW workspace diff and surface its ref
// in dr.SnapshotRef; a non-eligible step (Snapshot: "") must capture nothing.
//
// Scaffolding mirrors TestRunAgentCarriesMetrics (dispatch_metrics_test.go): a
// scripted fake agent registered in an agent.Registry, dispatched through a
// LocalDispatcher. The only additions are a blobs-backed fake container (so
// Snapshot succeeds rather than returning ErrUnsupported) and Snapshot:"workspace"
// on the ResolvedInputs — mirroring the code-step test.
func TestDispatcherCapturesSnapshotOnEligibleAgentStep(t *testing.T) {
	ctx := context.Background()

	blobs := state.NewInMemoryBlobs()
	cfake := container.NewFake().WithBlobs(blobs)
	h, err := cfake.Create(ctx, container.ContainerSpec{Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Scripted agent that succeeds (exit 0, valid output) → runAgent yields
	// OutcomeOK, so the snapshot block runs. A failing agent would make this
	// test vacuous (no snapshot attempted), so the success script is load-bearing.
	fake := agentfake.New("test/agent").Script(0, agentfake.Result{
		Output: map[string]any{"ok": true},
	})
	reg := &agent.Registry{}
	if err := reg.Register(fake); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d := &engine.LocalDispatcher{Backend: cfake, Handles: map[string]container.Handle{"ws": h}, Resolver: reg}
	intent := engine.NodeIntent{
		Path:           "a1",
		Node:           &ir.AgentStep{ID: "a1", Container: "ws", Uses: "test/agent"},
		ResolvedInputs: engine.ResolvedInputs{Uses: "test/agent", Snapshot: "workspace"},
	}

	dr, ch, err := d.Run(ctx, intent)
	for range ch {
	}
	if err != nil || dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Run: outcome=%q err=%v", dr.Outcome, err)
	}
	if dr.SnapshotRef == "" || dr.Container != "ws" {
		t.Errorf("dr = {ref:%q, container:%q}, want non-empty ref + ws", dr.SnapshotRef, dr.Container)
	}

	// Non-eligible step must NOT capture.
	intent.ResolvedInputs.Snapshot = ""
	dr2, ch2, _ := d.Run(ctx, intent)
	for range ch2 {
	}
	if dr2.SnapshotRef != "" {
		t.Errorf("non-eligible SnapshotRef = %q, want empty", dr2.SnapshotRef)
	}
}
