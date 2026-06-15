package engine_test

import (
	"context"
	"errors"
	"strings"
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

// TestRunAgent_EmptyContainer_NonContainerless_Permanent verifies the
// defense-in-depth guard added in runAgent: a non-containerless adapter
// (Containerless=false, the default) paired with an empty container: ref must
// return OutcomePermanentFailure with *agent.ErrInvalidConfig — NOT propagate
// to Backend.Exec (where the zero Handle causes a deep, mis-classified error).
// This covers map-body steps and any path that bypasses the run-start
// cli/runtimes.go walk.
func TestRunAgent_EmptyContainer_NonContainerless_Permanent(t *testing.T) {
	ctx := context.Background()

	// Default Caps: NativeSchema:true, Containerless:false — this adapter
	// requires a container. Pair it with an empty Container field to trigger
	// the guard. Script slot 0 so Launch would succeed IF the guard were absent.
	fk := agentfake.New("anthropic/claude-code").
		Script(0, agentfake.Result{Output: map[string]any{"answer": "42"}})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d := &engine.LocalDispatcher{
		Resolver: &reg,
		Handles:  map[string]container.Handle{},
	}
	intent := engine.NodeIntent{
		Path: "map[0].body[0]",
		Node: &ir.AgentStep{ID: "gen", Uses: "anthropic/claude-code", Container: ""},
		ResolvedInputs: engine.ResolvedInputs{
			Uses: "anthropic/claude-code",
			With: ir.RawConfig{"prompt": "hello"},
		},
	}

	dr, ch, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Run returned engine-level error: %v (want nil — guard must surface via dr.Outcome)", err)
	}
	for range ch {
	}
	if dr.Outcome != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (empty container + non-containerless adapter must be permanent)", dr.Outcome, engine.OutcomePermanentFailure)
	}
	var configErr *agent.ErrInvalidConfig
	if !errors.As(dr.Err, &configErr) {
		t.Fatalf("dr.Err = %v (%T), want *agent.ErrInvalidConfig", dr.Err, dr.Err)
	}
}

// TestRunAgent_Thread_NonThreaded_Permanent is the defense-in-depth guard for
// engine-threaded conversations (continues:). If inv.Thread is non-empty but the
// adapter's Caps.Threaded == false, the dispatcher must return
// OutcomePermanentFailure with *agent.ErrInvalidConfig — never silently drop the
// thread and call Launch. Mirrors the Containerless guard in the same function.
func TestRunAgent_Thread_NonThreaded_Permanent(t *testing.T) {
	ctx := context.Background()

	// Default fake caps: NativeSchema:true, Containerless:false, Threaded:false.
	// Pairing it with a non-empty Thread must trigger the guard.
	fk := agentfake.New("anthropic/claude-code").
		Script(0, agentfake.Result{Output: map[string]any{"answer": "42"}})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cfake := container.NewFake()
	h, err := cfake.Create(ctx, container.ContainerSpec{Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	d := &engine.LocalDispatcher{
		Backend:  cfake,
		Resolver: &reg,
		Handles:  map[string]container.Handle{"ws": h},
	}
	intent := engine.NodeIntent{
		Path: "turn2",
		Node: &ir.AgentStep{ID: "turn2", Uses: "anthropic/claude-code", Container: "ws"},
		ResolvedInputs: engine.ResolvedInputs{
			Uses:   "anthropic/claude-code",
			With:   ir.RawConfig{"prompt": "hi"},
			Thread: []agent.ThreadTurn{{User: "u1", Assistant: "a1"}}, // non-empty
		},
	}

	dr, ch, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Run returned engine-level error: %v (want nil — guard must surface via dr.Outcome)", err)
	}
	for range ch {
	}
	if dr.Outcome != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (non-empty Thread + non-Threaded adapter must be permanent)", dr.Outcome, engine.OutcomePermanentFailure)
	}
	var configErr *agent.ErrInvalidConfig
	if !errors.As(dr.Err, &configErr) {
		t.Fatalf("dr.Err = %v (%T), want *agent.ErrInvalidConfig", dr.Err, dr.Err)
	}
}

// TestRunAgent_Thread_ThreadedAdapter_OK verifies the positive path: a non-empty
// Thread paired with a Threaded adapter must pass through the guard cleanly and
// return OutcomeOK (the guard must NOT fire when Caps.Threaded == true).
func TestRunAgent_Thread_ThreadedAdapter_OK(t *testing.T) {
	ctx := context.Background()

	// Explicitly set Threaded:true — the guard must let this through.
	fk := agentfake.New("awf/llm").
		WithCaps(agent.Caps{NativeSchema: false, Containerless: true, Threaded: true}).
		Script(0, agentfake.Result{Output: map[string]any{"answer": "42"}})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d := &engine.LocalDispatcher{
		Resolver: &reg,
		Handles:  map[string]container.Handle{},
	}
	intent := engine.NodeIntent{
		Path: "turn2",
		Node: &ir.AgentStep{ID: "turn2", Uses: "awf/llm", Container: ""},
		ResolvedInputs: engine.ResolvedInputs{
			Uses:   "awf/llm",
			With:   ir.RawConfig{"model": "m", "prompt": "hi"},
			Thread: []agent.ThreadTurn{{User: "u1", Assistant: "a1"}}, // non-empty
		},
	}

	dr, ch, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	for range ch {
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q (Thread + Threaded adapter must pass guard)", dr.Outcome, engine.OutcomeOK)
	}
}

func TestRunAgent_ContextEvidence_UnsupportedAdapter_Permanent(t *testing.T) {
	ctx := context.Background()
	fk := agentfake.New("awf/llm").WithCaps(agent.Caps{Containerless: true}).
		Script(0, agentfake.Result{Output: map[string]any{"answer": "42"}})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := &engine.LocalDispatcher{Resolver: &reg, Handles: map[string]container.Handle{}}
	intent := engine.NodeIntent{
		Path: "gate[0].attempt-1.evaluate.judge",
		Node: &ir.AgentStep{ID: "judge", Uses: "awf/llm", Container: ""},
		ResolvedInputs: engine.ResolvedInputs{
			Uses:            "awf/llm",
			With:            ir.RawConfig{"model": "m", "prompt": "hi"},
			ContextEvidence: []agent.ThreadTurn{{User: "u1", Assistant: "a1"}},
		},
	}
	dr, ch, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Run returned engine-level error: %v", err)
	}
	for range ch {
	}
	if dr.Outcome != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q", dr.Outcome, engine.OutcomePermanentFailure)
	}
	var configErr *agent.ErrInvalidConfig
	if !errors.As(dr.Err, &configErr) {
		t.Fatalf("dr.Err = %v (%T), want *agent.ErrInvalidConfig", dr.Err, dr.Err)
	}
}

func TestRunAgent_ContextEvidence_ContextEvidenceAdapter_OK(t *testing.T) {
	ctx := context.Background()
	fk := agentfake.New("awf/llm").
		WithCaps(agent.Caps{NativeSchema: false, Containerless: true, ContextEvidence: true}).
		Script(0, agentfake.Result{Output: map[string]any{"answer": "42"}})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := &engine.LocalDispatcher{Resolver: &reg, Handles: map[string]container.Handle{}}
	intent := engine.NodeIntent{
		Path: "gate[0].attempt-1.evaluate.judge",
		Node: &ir.AgentStep{ID: "judge", Uses: "awf/llm", Container: ""},
		ResolvedInputs: engine.ResolvedInputs{
			Uses:            "awf/llm",
			With:            ir.RawConfig{"model": "m", "prompt": "hi"},
			ContextEvidence: []agent.ThreadTurn{{User: "u1", Assistant: "a1"}},
		},
	}
	dr, ch, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	for range ch {
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", dr.Outcome, engine.OutcomeOK)
	}
	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("Calls len = %d, want 1", len(calls))
	}
	if len(calls[0].ContextEvidence) != 1 || calls[0].ContextEvidence[0].User != "u1" {
		t.Fatalf("ContextEvidence = %+v, want propagated turn", calls[0].ContextEvidence)
	}
}

// TestRunAgent_Containerless_PassesInputFiles verifies that runAgent threads
// the resolved containerless input_files (ResolvedInputs.ContainerlessFiles)
// into the AgentInvocation it hands the adapter. Task 2 populated
// ContainerlessFiles for containerless steps; Task 3 wires it into the
// AgentInvocation so a containerless awf/llm step's files actually reach Launch.
//
// The fake adapter records every AgentInvocation via Calls(); we dispatch
// through the public Run and assert the recorded invocation carries the file.
func TestRunAgent_Containerless_PassesInputFiles(t *testing.T) {
	ctx := context.Background()

	fk := agentfake.New("awf/llm").WithCaps(agent.Caps{Containerless: true}).
		Script(0, agentfake.Result{Output: map[string]any{"answer": "42"}})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d := &engine.LocalDispatcher{
		Resolver: &reg,
		Handles:  map[string]container.Handle{},
	}
	intent := engine.NodeIntent{
		Path: "graph[0]",
		Node: &ir.AgentStep{ID: "ask", Uses: "awf/llm", Container: ""},
		ResolvedInputs: engine.ResolvedInputs{
			Uses:               "awf/llm",
			With:               ir.RawConfig{"model": "m", "prompt": "hi"},
			ContainerlessFiles: []agent.InputFile{{Name: "doc", MIME: "application/pdf", Content: []byte("%PDF")}},
		},
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

	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("Launch calls = %d, want 1", len(calls))
	}
	got := calls[0].InputFiles
	if len(got) != 1 || got[0].Name != "doc" {
		t.Fatalf("adapter did not receive InputFiles: %+v", got)
	}
	if got[0].MIME != "application/pdf" || string(got[0].Content) != "%PDF" {
		t.Fatalf("InputFile content/MIME not threaded verbatim: %+v", got[0])
	}
}

func TestEngineRejectsPersistentEvaluateBeforeLaunch(t *testing.T) {
	ctx := context.Background()

	fk := agentfake.New("live/agent").
		WithCaps(agent.Caps{Containerless: true, PersistentSession: true}).
		Script(0, agentfake.Result{Output: map[string]any{"answer": "42"}})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d := &engine.LocalDispatcher{
		Resolver: &reg,
		Handles:  map[string]container.Handle{},
	}
	intent := engine.NodeIntent{
		Path:           "gate[0].attempt-1.evaluate[0]",
		Node:           &ir.AgentStep{ID: "judge", Uses: "live/agent"},
		IsGateEvaluate: true,
		ResolvedInputs: engine.ResolvedInputs{
			Uses: "live/agent",
			With: ir.RawConfig{"prompt": "judge"},
		},
	}

	dr, ch, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Run returned engine-level error: %v", err)
	}
	for range ch {
	}
	if dr.Outcome != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q", dr.Outcome, engine.OutcomePermanentFailure)
	}
	var configErr *agent.ErrInvalidConfig
	if !errors.As(dr.Err, &configErr) {
		t.Fatalf("dr.Err = %v (%T), want *agent.ErrInvalidConfig", dr.Err, dr.Err)
	}
	if len(fk.Calls()) != 0 {
		t.Fatalf("Launch calls = %d, want 0", len(fk.Calls()))
	}
}

func TestRunAgentCopiesLiveDispatchMetadata(t *testing.T) {
	ctx := context.Background()
	live := &agent.LiveDispatch{
		AdapterRef:     "openai/codex-live",
		SessionKey:     "builder",
		SessionKeyHash: "sha256:session",
		LeaseID:        "lease-1",
		ActiveTurnID:   "turn-intent-1",
		ProviderTurnID: "provider-turn-1",
		RunID:          "run-1",
		NodePath:       "build",
		Epoch:          2,
		CommittedUnix:  1_781_114_500,
	}
	fk := agentfake.New("openai/codex-live").
		WithCaps(agent.Caps{NativeSchema: true, Containerless: true, PersistentSession: true}).
		Script(0, agentfake.Result{Output: map[string]any{"ok": true}, Live: live})
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := &engine.LocalDispatcher{Resolver: &reg, Handles: map[string]container.Handle{}}
	intent := engine.NodeIntent{
		Path: "build",
		Node: &ir.AgentStep{ID: "build", Uses: "openai/codex-live"},
		RunContext: agent.RunContext{
			RunID:        "run-1",
			CurrentEpoch: 1,
			NextEpoch:    2,
		},
		ResolvedInputs: engine.ResolvedInputs{
			Uses: "openai/codex-live",
			With: ir.RawConfig{"prompt": "hi", "cwd": t.TempDir(), "session": "builder"},
		},
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
	if dr.Live == nil {
		t.Fatal("DispatchResult.Live = nil, want metadata copied")
	}
	if got, want := *dr.Live, (engine.LiveDispatchRecord{
		AdapterRef:     live.AdapterRef,
		SessionKey:     live.SessionKey,
		SessionKeyHash: live.SessionKeyHash,
		LeaseID:        live.LeaseID,
		ActiveTurnID:   live.ActiveTurnID,
		ProviderTurnID: live.ProviderTurnID,
		RunID:          live.RunID,
		NodePath:       live.NodePath,
		Epoch:          live.Epoch,
		CommittedUnix:  live.CommittedUnix,
	}); got != want {
		t.Fatalf("DispatchResult.Live = %+v, want %+v", got, want)
	}
}

func TestRunAgent_OutputFileContractInvalidArtifactRetryable(t *testing.T) {
	ctx := context.Background()

	cfake := container.NewFake()
	h, err := cfake.Create(ctx, container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := cfake.WriteFile(h, "/out/report.json", []byte(`{"count":"not-an-integer"}`)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fk := agentfake.New("test/agent").Script(0, agentfake.Result{Output: map[string]any{"ok": true}})
	reg := &agent.Registry{}
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	schema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"count"},
		"properties": map[string]any{
			"count": map[string]any{"type": "integer"},
		},
	}
	d := &engine.LocalDispatcher{
		Backend:  cfake,
		Handles:  map[string]container.Handle{"lab": h},
		Resolver: reg,
	}
	intent := engine.NodeIntent{
		Path: "agent_report",
		Node: &ir.AgentStep{ID: "agent_report", Container: "lab", Uses: "test/agent"},
		ResolvedInputs: engine.ResolvedInputs{
			Uses:        "test/agent",
			OutputFiles: []string{"/out/report.json"},
			OutputFileContracts: map[string]engine.OutputFileContract{
				"/out/report.json": {Format: "json", Schema: schema},
			},
		},
	}

	dr, ch, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Run returned engine-level error: %v", err)
	}
	for range ch {
	}
	if dr.Outcome != engine.OutcomeRetryableFailure {
		t.Fatalf("Outcome = %q, want %q (Err: %v)", dr.Outcome, engine.OutcomeRetryableFailure, dr.Err)
	}
	if dr.Err == nil || !strings.Contains(dr.Err.Error(), "artifact contract") || !strings.Contains(dr.Err.Error(), "schema validation") {
		t.Fatalf("Err = %v, want artifact contract schema validation failure", dr.Err)
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
