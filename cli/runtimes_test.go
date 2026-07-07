package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// intPtr is a small test helper for *int struct fields (e.g. ir.Map.Concurrency, F45
// presence-tracking) — shared by this file and cli/runtimeimageguard_test.go, both
// `package cli`.
func intPtr(n int) *int { return &n }

func TestWalkAgentRefs_EmptyGraph(t *testing.T) {
	wf := &ir.Workflow{}
	got := walkAgentRefs(wf)
	if len(got) != 0 {
		t.Errorf("walkAgentRefs(empty) = %v, want empty", got)
	}
}

func TestWalkAgentRefs_CodeStepOnly(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "a", Container: "lab", Run: "echo"},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 0 {
		t.Errorf("walkAgentRefs(code-only) = %v, want empty (no `uses:` refs)", got)
	}
}

func TestWalkAgentRefs_TopLevelAgentStep(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "triage", Container: "lab", Uses: "anthropic/claude-code"},
		},
	}
	got := walkAgentRefs(wf)
	want := []agentRef{{Uses: "anthropic/claude-code", Container: "lab"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkAgentRefs = %v, want %v", got, want)
	}
}

func TestWalkAgentRefs_Deduplicated(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "a", Container: "lab", Uses: "anthropic/claude-code"},
			&ir.AgentStep{ID: "b", Container: "lab", Uses: "anthropic/claude-code"},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 1 {
		t.Errorf("len(walkAgentRefs) = %d, want 1 (deduplicated)", len(got))
	}
}

func TestWalkAgentRefs_DistinctContainers(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "a", Container: "lab", Uses: "anthropic/claude-code"},
			&ir.AgentStep{ID: "b", Container: "scratch", Uses: "anthropic/claude-code"},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 2 {
		t.Fatalf("len(walkAgentRefs) = %d, want 2 (distinct containers)", len(got))
	}
	// Results MUST be sorted by (Uses, Container) for deterministic golden tests.
	want := []agentRef{
		{Uses: "anthropic/claude-code", Container: "lab"},
		{Uses: "anthropic/claude-code", Container: "scratch"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkAgentRefs = %v, want %v (sorted)", got, want)
	}
}

func TestWalkAgentRefs_NestedInGate(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Gate{
				Generate: ir.NodeList{
					&ir.AgentStep{ID: "g", Container: "lab", Uses: "anthropic/claude-code"},
				},
				Evaluate: ir.NodeList{
					&ir.AgentStep{ID: "e", Container: "lab", Uses: "anthropic/claude-code"},
				},
				Until:       "evaluate.ok",
				MaxAttempts: 3,
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 1 {
		t.Errorf("len(walkAgentRefs nested-in-gate) = %d, want 1 (same ref + container, dedup)", len(got))
	}
}

func TestWalkAgentRefs_NestedInTryCatchFinally(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Try{
				Do:      ir.NodeList{&ir.AgentStep{ID: "d", Container: "lab", Uses: "x"}},
				Catch:   ir.NodeList{&ir.AgentStep{ID: "c", Container: "lab", Uses: "y"}},
				Finally: ir.NodeList{&ir.AgentStep{ID: "f", Container: "lab", Uses: "z"}},
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 3 {
		t.Errorf("len(walkAgentRefs try/catch/finally) = %d, want 3 (x/y/z)", len(got))
	}
}

func TestWalkAgentRefs_NestedInParallelOnly(t *testing.T) {
	// NOTE: ir.Parallel has a single `Children NodeList` field (flat list — the
	// standard's `{"parallel":[<node>,...]}` shape). Each child is conceptually
	// a branch. No nested [][]Node.
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Parallel{
				Children: ir.NodeList{
					&ir.AgentStep{ID: "p1", Container: "lab", Uses: "p"},
				},
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 1 {
		t.Errorf("len(walkAgentRefs parallel) = %d, want 1", len(got))
	}
}

// TestWalkAgentRefs_MapBodyIncluded pins that static/containerless map-body
// AgentSteps are part of the runtime pin set. Only runtime-created map.image
// container aliases are skipped because no handle exists at run start.
func TestWalkAgentRefs_MapBodyIncluded(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Map{
				Over:        "input.items",
				As:          "item",
				Container:   "lab",
				Concurrency: intPtr(1),
				Body:        ir.NodeList{&ir.AgentStep{ID: "m1", Container: "lab", Uses: "m"}},
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 1 {
		t.Fatalf("len(walkAgentRefs map-body-only) = %d, want 1", len(got))
	}
	want := agentRef{Uses: "m", Container: "lab"}
	if got[0] != want {
		t.Errorf("walkAgentRefs map-body-only = %+v, want %+v", got[0], want)
	}
}

func TestWalkAgentRefs_MapRuntimeImageContainerAliasSkipped(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Map{
				Over:      "input.items",
				As:        "item",
				Container: "lab",
				Image:     "oci://repo/{{ item.image }}",
				Body: ir.NodeList{
					&ir.AgentStep{ID: "inside", Container: "lab", Uses: "runtime-image-agent"},
					&ir.AgentStep{ID: "containerless", Uses: "awf/llm"},
				},
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 1 {
		t.Fatalf("len = %d, want one containerless ref", len(got))
	}
	if got[0].Uses != "awf/llm" || got[0].Container != "" {
		t.Errorf("walkAgentRefs = %+v, want only awf/llm containerless", got)
	}
}

// TestWalkAgentRefs_TopLevelAndMapSibling — when a workflow has BOTH a top-level
// AgentStep AND a static Map-body AgentStep, both are pinned.
func TestWalkAgentRefs_TopLevelAndMapSibling(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "top", Container: "lab", Uses: "anthropic/claude-code"},
			&ir.Map{
				Over:      "input.items",
				As:        "item",
				Container: "lab",
				Body:      ir.NodeList{&ir.AgentStep{ID: "inside", Container: "lab", Uses: "should-be-skipped"}},
			},
		},
	}
	got := walkAgentRefs(wf)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (top-level + static map body)", len(got))
	}
	if got[0].Uses != "anthropic/claude-code" {
		t.Errorf("got[0].Uses = %q, want %q", got[0].Uses, "anthropic/claude-code")
	}
	if got[1].Uses != "should-be-skipped" {
		t.Errorf("got[1].Uses = %q, want static map-body ref", got[1].Uses)
	}
}

// NOTE: There is intentionally no TestWalkAgentRefs_UnknownNodePanics test.
// ir.Node is interface{ isNode() } (see ir/node.go:17) — a closed sum type
// with an UNEXPORTED marker method. No type outside the ir package can
// satisfy ir.Node, so the runtime-ref traversal's default arm is unreachable
// from cli/_test. The panic is defensive documentation only; if a future ir/
// node type lands without updating that switch, integration tests exercising
// the new node panic loudly.

func TestResolveRuntimes_Empty(t *testing.T) {
	got, err := resolveRuntimes(context.Background(), nil, &agent.Registry{}, nil)
	if err != nil {
		t.Fatalf("resolveRuntimes(nil refs): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestResolveRuntimes_Found(t *testing.T) {
	var r agent.Registry
	f := fake.New("anthropic/claude-code").WithVersion("2.1.118")
	if err := r.Register(f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	refs := []agentRef{{Uses: "anthropic/claude-code", Container: "lab"}}
	handles := map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}}
	got, err := resolveRuntimes(context.Background(), refs, &r, handles)
	if err != nil {
		t.Fatalf("resolveRuntimes: %v", err)
	}
	want := []engine.ResolvedRuntime{
		{Ref: "anthropic/claude-code", Version: "2.1.118", Container: "lab"},
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveRuntimes_AdapterNotFound(t *testing.T) {
	refs := []agentRef{{Uses: "anthropic/claude-code", Container: "lab"}}
	_, err := resolveRuntimes(context.Background(), refs, &agent.Registry{}, nil)
	var target *agent.ErrAdapterNotFound
	if !errors.As(err, &target) {
		t.Fatalf("err = %v, want *ErrAdapterNotFound", err)
	}
	if target.Ref != "anthropic/claude-code" {
		t.Errorf("Ref = %q, want %q", target.Ref, "anthropic/claude-code")
	}
}

func TestResolveRuntimes_HandleMissing(t *testing.T) {
	// If the CLI somehow doesn't have a handle for a container the ref names,
	// resolveRuntimes should error — but resolveRuntimes can't be the one that
	// CREATES handles (that's slice 5.2 dispatch's job in this slice's wiring).
	// The error here is a "programmer bug at the wiring level" class.
	var r agent.Registry
	if err := r.Register(fake.New("x")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	refs := []agentRef{{Uses: "x", Container: "missing"}}
	_, err := resolveRuntimes(context.Background(), refs, &r, map[string]container.Handle{})
	if err == nil {
		t.Fatalf("err = nil, want non-nil (missing handle)")
	}
}

// TestResolveRuntimes_DriftIfVersionChanges asserts that two consecutive
// resolveRuntimes calls with different versions return different
// ResolvedRuntimes (the *comparison* is done by cli/resume.go; this test
// validates the building blocks).
func TestResolveRuntimes_DriftIfVersionChanges(t *testing.T) {
	var r agent.Registry
	f := fake.New("x").WithVersion("v1")
	if err := r.Register(f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	refs := []agentRef{{Uses: "x", Container: "lab"}}
	handles := map[string]container.Handle{"lab": {Name: "lab", ID: "fake-1"}}
	first, err := resolveRuntimes(context.Background(), refs, &r, handles)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Mutate the fake's version (simulates a binary upgrade between run and resume).
	f.WithVersion("v2")
	second, err := resolveRuntimes(context.Background(), refs, &r, handles)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first[0].Version == second[0].Version {
		t.Errorf("expected drift: first.Version = %q, second.Version = %q — should differ", first[0].Version, second[0].Version)
	}
	if first[0].Version != "v1" || second[0].Version != "v2" {
		t.Errorf("versions = (%q, %q), want (v1, v2)", first[0].Version, second[0].Version)
	}
}

func TestCheckRuntimesDrift_NoChange(t *testing.T) {
	recorded := []engine.ResolvedRuntime{{Ref: "x", Version: "v1", Container: "lab"}}
	current := []engine.ResolvedRuntime{{Ref: "x", Version: "v1", Container: "lab"}}
	if err := checkRuntimesDrift(recorded, current); err != nil {
		t.Errorf("no drift: err = %v, want nil", err)
	}
}

func TestCheckRuntimesDrift_VersionChanged(t *testing.T) {
	recorded := []engine.ResolvedRuntime{{Ref: "x", Version: "v1", Container: "lab"}}
	current := []engine.ResolvedRuntime{{Ref: "x", Version: "v2", Container: "lab"}}
	err := checkRuntimesDrift(recorded, current)
	var target *ErrRuntimeDrift
	if !errors.As(err, &target) {
		t.Fatalf("err = %v, want *ErrRuntimeDrift", err)
	}
	if target.Recorded != "v1" || target.Current != "v2" {
		t.Errorf("drift mismatch: %+v", target)
	}
}

func TestCheckRuntimesDrift_RuntimeAdded(t *testing.T) {
	recorded := []engine.ResolvedRuntime{{Ref: "x", Version: "v1", Container: "lab"}}
	current := []engine.ResolvedRuntime{
		{Ref: "x", Version: "v1", Container: "lab"},
		{Ref: "y", Version: "v1", Container: "lab"},
	}
	err := checkRuntimesDrift(recorded, current)
	if err == nil {
		t.Fatalf("err = nil, want non-nil (runtime added)")
	}
}

func TestResolveRuntimes_ContainerlessAdapter_NoHandleNeeded(t *testing.T) {
	fk := fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true}).WithVersion("awf-llm/1")
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	refs := []agentRef{{Uses: "awf/llm", Container: ""}}
	got, err := resolveRuntimes(context.Background(), refs, &reg, map[string]container.Handle{}) // no handles
	if err != nil {
		t.Fatalf("resolveRuntimes: %v", err)
	}
	if len(got) != 1 || got[0].Ref != "awf/llm" || got[0].Version != "awf-llm/1" || got[0].Container != "" {
		t.Fatalf("got %+v, want one {awf/llm, awf-llm/1, \"\"}", got)
	}
}

func TestRuntimeResolutionSupportsLiveAdapterRefsWithoutRuntimeCommand(t *testing.T) {
	var reg agent.Registry
	codexLive := fake.New("openai/codex-live").
		WithCaps(agent.Caps{NativeSchema: true, Containerless: true, PersistentSession: true}).
		WithVersion("codex-live/0")
	gooseLive := fake.New("block/goose-live").
		WithCaps(agent.Caps{Containerless: true, PersistentSession: true}).
		WithVersion("goose-live/0")
	for _, adapter := range []agent.Adapter{gooseLive, codexLive} {
		if err := reg.Register(adapter); err != nil {
			t.Fatalf("Register %q: %v", adapter.Ref(), err)
		}
	}

	refs := []agentRef{
		{Uses: "block/goose-live"},
		{Uses: "openai/codex-live"},
	}
	got, err := resolveRuntimes(context.Background(), refs, &reg, map[string]container.Handle{})
	if err != nil {
		t.Fatalf("resolveRuntimes: %v", err)
	}
	want := []engine.ResolvedRuntime{
		{Ref: "block/goose-live", Version: "goose-live/0"},
		{Ref: "openai/codex-live", Version: "codex-live/0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveRuntimes = %+v, want %+v", got, want)
	}
}

func TestResolveRuntimes_NonContainerlessAdapter_NoContainer_Errors(t *testing.T) {
	fk := fake.New("anthropic/claude-code") // default Caps: Containerless false
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	refs := []agentRef{{Uses: "anthropic/claude-code", Container: ""}}
	_, err := resolveRuntimes(context.Background(), refs, &reg, map[string]container.Handle{})
	var want *ErrContainerRequired
	if !errors.As(err, &want) || want.Ref != "anthropic/claude-code" {
		t.Fatalf("err = %v, want *ErrContainerRequired{Ref:anthropic/claude-code}", err)
	}
}

func TestCheckRuntimesDrift_RuntimeRemoved(t *testing.T) {
	recorded := []engine.ResolvedRuntime{
		{Ref: "x", Version: "v1", Container: "lab"},
		{Ref: "y", Version: "v1", Container: "lab"},
	}
	current := []engine.ResolvedRuntime{{Ref: "x", Version: "v1", Container: "lab"}}
	err := checkRuntimesDrift(recorded, current)
	if err == nil {
		t.Fatalf("err = nil, want non-nil (runtime removed)")
	}
}
