package engine_test

// F4a: container-less `run:` via a per-step implicit host-workspace handle.
//
// These tests prove the two things ir's relaxed validation (CodeStep no
// longer requires container:) needs from the runtime side:
//
//   - TestRunBareCodeStepExecutesWithFakeBackend: a bare `run:` step (no
//     declared container:) actually dispatches and captures output — the
//     interpreter provisions a per-step implicit handle at dispatch
//     (engine.BareRunHandleKey / hostWorkspaceSpec), mirroring
//     engine/map.go's dispatchItem per-item provisioning.
//   - TestRunParallelBareRunStepsAreIsolated: two bare steps running under
//     Parallel each get their OWN handle (distinct Backend.Create calls,
//     distinct handle IDs, no cross-write) — proving
//     checkParallelDistinctContainers' skip of ctr=="" (ir/validate_structural.go)
//     is safe.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

func TestRunBareCodeStepExecutesWithFakeBackend(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExecWithFiles("./bare.sh", container.ExecResult{
		ExitCode: 0,
		Stdout:   []byte("bare-hello\n"),
	}, nil, map[string][]byte{"out.txt": []byte("bare-content")})

	// No container: — the interpreter must provision a per-step implicit
	// host-workspace handle; nothing pre-populates disp.Handles for this step
	// (unlike "lab", which newRunHarness seeds for the declared-container case).
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "bare", Run: "./bare.sh", OutputFiles: ir.OutputFiles{{Path: "out.txt"}}},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}
	nr, ok := rs.Completed["bare"]
	if !ok {
		t.Fatal("RunState.Completed missing 'bare'")
	}
	if nr.Outcome != engine.OutcomeOK || string(nr.Stdout) != "bare-hello\n" {
		t.Errorf("nr = %+v, want ok + stdout 'bare-hello'", nr)
	}
	ref, ok := nr.Files["out.txt"]
	if !ok {
		t.Fatal("nr.Files missing 'out.txt'")
	}
	content, err := blobs.Get(ref)
	if err != nil {
		t.Fatalf("blobs.Get(%q): %v", ref, err)
	}
	if string(content) != "bare-content" {
		t.Errorf("out.txt content = %q, want %q", string(content), "bare-content")
	}

	// A per-step implicit handle was actually provisioned and torn down —
	// not a silent no-op / reuse of the pre-seeded "lab" handle. newRunHarness
	// itself already Create'd "lab" before Run() runs, so this step's implicit
	// Create is CreateSpecs[1], not [0].
	if len(fake.CreateSpecs) != 2 {
		t.Fatalf("CreateSpecs len = %d, want 2 (harness's 'lab' + one per-step implicit Create)", len(fake.CreateSpecs))
	}
	if got, want := fake.CreateSpecs[1].Name, "_run.bare"; got != want {
		t.Errorf("CreateSpecs[1].Name = %q, want %q (hostWorkspaceSpec keyed by node path)", got, want)
	}
	if fake.CreateSpecs[1].Image != "" {
		t.Errorf("CreateSpecs[1].Image = %q, want empty (host workspace, no image)", fake.CreateSpecs[1].Image)
	}
	if len(fake.DestroyCalls) != 1 {
		t.Errorf("DestroyCalls len = %d, want 1 (per-step handle torn down after the step)", len(fake.DestroyCalls))
	}

	events, _ := log.Fold()
	var sawCompleted bool
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted && e.Path == "bare" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Errorf("no node.completed event for 'bare'; events: %+v", events)
	}
}

func TestRunParallelBareRunStepsAreIsolated(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExecWithFiles("./produce_b0.sh", container.ExecResult{ExitCode: 0}, nil,
		map[string][]byte{"out.txt": []byte("b0-content")})
	fake.ProgramExecWithFiles("./produce_b1.sh", container.ExecResult{ExitCode: 0}, nil,
		map[string][]byte{"out.txt": []byte("b1-content")})

	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.Parallel{Children: ir.NodeList{
			&ir.CodeStep{ID: "b0", Run: "./produce_b0.sh", OutputFiles: ir.OutputFiles{{Path: "out.txt"}}},
			&ir.CodeStep{ID: "b1", Run: "./produce_b1.sh", OutputFiles: ir.OutputFiles{{Path: "out.txt"}}},
		}},
	}}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok", oc)
	}

	nr0, ok := rs.Completed["parallel[0].b0"]
	if !ok {
		t.Fatal("RunState.Completed missing 'parallel[0].b0'")
	}
	nr1, ok := rs.Completed["parallel[0].b1"]
	if !ok {
		t.Fatal("RunState.Completed missing 'parallel[0].b1'")
	}
	// Each branch's own output — a shared/collided handle would let the
	// later Exec's produced file overwrite the earlier one's "out.txt" entry.
	ref0, ok := nr0.Files["out.txt"]
	if !ok {
		t.Fatal("nr0.Files missing 'out.txt'")
	}
	ref1, ok := nr1.Files["out.txt"]
	if !ok {
		t.Fatal("nr1.Files missing 'out.txt'")
	}
	content0, err := blobs.Get(ref0)
	if err != nil {
		t.Fatalf("blobs.Get(b0 ref): %v", err)
	}
	content1, err := blobs.Get(ref1)
	if err != nil {
		t.Fatalf("blobs.Get(b1 ref): %v", err)
	}
	if string(content0) != "b0-content" {
		t.Errorf("b0 out.txt = %q, want %q (cross-write from b1?)", string(content0), "b0-content")
	}
	if string(content1) != "b1-content" {
		t.Errorf("b1 out.txt = %q, want %q (cross-write from b0?)", string(content1), "b1-content")
	}

	// Two distinct per-step Creates, not one shared handle. newRunHarness
	// itself already Create'd "lab" before Run() runs, so the two per-step
	// implicit Creates are CreateSpecs[1] and [2], not [0] and [1].
	if len(fake.CreateSpecs) != 3 {
		t.Fatalf("CreateSpecs len = %d, want 3 (harness's 'lab' + one per bare step)", len(fake.CreateSpecs))
	}
	bareSpecs := fake.CreateSpecs[1:]
	if bareSpecs[0].Name == bareSpecs[1].Name {
		t.Errorf("CreateSpecs Name collision: both %q", bareSpecs[0].Name)
	}
	wantNames := map[string]bool{"_run.parallel[0].b0": true, "_run.parallel[0].b1": true}
	for _, spec := range bareSpecs {
		if !wantNames[spec.Name] {
			t.Errorf("unexpected CreateSpecs Name %q, want one of %v", spec.Name, wantNames)
		}
	}

	// Two distinct handle IDs actually used for Exec — the strongest proof
	// the two branches never dispatched against the same underlying handle.
	if len(fake.ExecHandles) != 2 {
		t.Fatalf("ExecHandles len = %d, want 2", len(fake.ExecHandles))
	}
	if fake.ExecHandles[0].ID == fake.ExecHandles[1].ID {
		t.Errorf("ExecHandles ID collision: both %q — bare steps shared ONE handle", fake.ExecHandles[0].ID)
	}

	// Both per-step handles torn down (mirrors dispatchItem's per-item Destroy).
	if len(fake.DestroyCalls) != 2 {
		t.Errorf("DestroyCalls len = %d, want 2", len(fake.DestroyCalls))
	}

	events, _ := log.Fold()
	seen := map[string]bool{}
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted {
			seen[e.Path] = true
		}
	}
	if !seen["parallel[0].b0"] || !seen["parallel[0].b1"] {
		t.Errorf("missing node.completed for one or both branches; seen=%v", seen)
	}
}

// F4b (e): a bare `run:` step never inherits snapshot:workspace capture from
// an unrelated sibling container declared elsewhere in the SAME workflow.
// runCode's capture gate (local_dispatcher.go: "intent.ResolvedInputs.Snapshot
// == \"workspace\"") reads intent.ResolvedInputs.Snapshot, which the
// interpreter resolves via wf.Containers[snapBare].Snapshot where
// snapBare = SplitContainerRef(cs.Container) — for a bare step cs.Container
// == "", so this is always a Go zero-value map miss (ir's containerNamePattern
// forbids an empty container name, so wf.Containers can never actually
// contain a "" key). This test proves that resolution end to end: "ws" here
// is a real, validly-shaped snapshot:workspace container that "bare" never
// references — if the bare step's resolution ever picked it up by mistake,
// the fake backend (no blobs injected — see newRunHarness) would fail the
// Snapshot call with container.ErrUnsupported and the run would end
// permanent_failure instead of ok.
func TestBareCodeStepNeverInheritsSiblingSnapshotWorkspace(t *testing.T) {
	t.Parallel()
	fake, _, disp, log, blobs, clk, rs := newRunHarness(t)
	fake.ProgramExec("./bare.sh", container.ExecResult{ExitCode: 0}, nil)

	wf := &ir.Workflow{
		Containers: map[string]ir.Container{
			"ws": {
				Image:    "oci://example.com/ws@sha256:" + strings.Repeat("0", 64),
				Snapshot: "workspace",
			},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "bare", Run: "./bare.sh"},
		},
	}
	def := &ir.LoadedDefinition{Workflow: wf}

	oc, err := engine.Run(context.Background(), def, rs, disp, log, blobs, clk, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok (a snapshot capture attempt against the unblobbed fake would fail closed)", oc)
	}

	events, _ := log.Fold()
	var sawCompleted bool
	for _, e := range events {
		if e.Type != engine.EventNodeCompleted || e.Path != "bare" {
			continue
		}
		sawCompleted = true
		var d engine.NodeCompletedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("unmarshal node.completed: %v", err)
		}
		if d.SnapshotRef != "" {
			t.Errorf("node.completed{bare}.SnapshotRef = %q, want empty — bare step must not capture a sibling container's snapshot:workspace", d.SnapshotRef)
		}
	}
	if !sawCompleted {
		t.Fatal("no node.completed event for 'bare'")
	}
}
