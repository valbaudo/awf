package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
)

// harness simulates run-then-resume against in-mem fakes. One per bucket
// sub-test; bucket calls runWorkflow then resumeWorkflow (with fault hooks
// programmed in between if needed).
//
// input is an optional pre-bound input map (Phase 3 slice 3.4): if non-nil,
// it's passed to engine.NewRunState on the FIRST run. Resume reads input
// back from the log's run.started entry via Fold; this field is only
// consulted on the first-run branch. Use newHarnessWithInput to set it.
//
// broker is non-nil only for Bucket 8 (signal) sub-tests. newHarnessWithBroker
// wires it; other buckets leave it nil. The baseDir field is the parent
// t.TempDir() shared by wfPath and the broker's control directory (M13).
type harness struct {
	wfPath  string
	baseDir string // M13: parent of wfPath; shared with broker controlDir
	clk     *clock.Fake
	log     *state.InMemoryLog
	blobs   *state.InMemoryBlobs
	factory BackendFactory
	runID   string
	input   map[string]any
	broker  *signal.Broker // slice 3.5 — nil for non-signal fixtures (most buckets)

	// runtimes is written into the FIRST-run run.started event's Runtimes field
	// (the agent-version pinning slice cli/runtimes.go populates on the real CLI
	// path). nil for every non-role bucket — `omitempty` then keeps run.started
	// byte-identical to those buckets. The roles bucket (testRoles) sets it to
	// the (ref, version, container) pairs it resolved through the registry, the
	// conformance equivalent of the CLI's resolveRuntimes walk, so it can assert
	// the role is a first-class pinned runtime. Consulted only on the first-run
	// branch (resume reads Runtimes back from the log via Fold).
	runtimes []engine.ResolvedRuntime

	// agentRegistry is ALWAYS non-nil. newHarness initializes it to an empty
	// *agent.Registry{} so the dispatcher's Resolver field never receives a
	// typed-nil interface (the Go nil-interface gotcha:
	// https://go.dev/doc/faq#nil_error — a *Registry(nil) assigned to
	// agent.Resolver compares != nil, then panics on Lookup). For non-agent
	// buckets the registry is empty and runAgent.Lookup misses cleanly;
	// agent buckets overwrite it via newHarnessWithAgentRegistry.
	agentRegistry *agent.Registry
}

func newHarness(t *testing.T, factory BackendFactory, workflowYAML string) *harness {
	t.Helper()
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "workflow.yaml")
	if err := os.WriteFile(wfPath, []byte(workflowYAML), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return &harness{
		wfPath:        wfPath,
		baseDir:       dir,
		clk:           clk,
		log:           state.NewInMemoryLog(clk),
		blobs:         state.NewInMemoryBlobs(),
		factory:       factory,
		runID:         "conformance-run",
		agentRegistry: &agent.Registry{},
	}
}

// newHarnessWithBroker derives controlDir from the harness's existing baseDir
// (M13 fix — single t.TempDir() parent for workflow + broker; cleaner layout).
// Bucket 8 (Task 15) uses this; other buckets pass nil broker.
func newHarnessWithBroker(t *testing.T, factory BackendFactory, workflowYAML string) *harness {
	t.Helper()
	h := newHarness(t, factory, workflowYAML)
	controlDir := filepath.Join(h.baseDir, "control")
	h.broker = signal.NewBroker(controlDir, signal.WithPollInterval(time.Millisecond))
	return h
}

// newHarnessWithInput is a variant of newHarness that pre-binds an input map
// to the RunState (passes through engine.NewRunState's input parameter).
// Slice 3.4 conformance Bucket 7 uses this for map fixtures whose `over`
// templates reference {{ input.<field> }}.
func newHarnessWithInput(t *testing.T, factory BackendFactory, workflowYAML string, input map[string]any) *harness {
	t.Helper()
	h := newHarness(t, factory, workflowYAML)
	h.input = input
	return h
}

// newHarnessWithAgentRegistry is the Bucket 12/13/15 constructor — base
// newHarness plus a pre-populated *agent.Registry the dispatcher consults
// at AgentStep dispatch. register is a setup callback that adds adapters
// to the registry (typically calling fake.New(ref).Script(...) chains and
// reg.Register on the result).
//
// Bucket 14 (slice 5.4) uses conformance.RunAgentSuite — a separate path
// from RunSuite — and constructs its registry with a real *agent/claude
// adapter; that's not this constructor.
func newHarnessWithAgentRegistry(t *testing.T, factory BackendFactory, workflowYAML string, register func(*agent.Registry)) *harness {
	t.Helper()
	h := newHarness(t, factory, workflowYAML) // h.agentRegistry is already a non-nil empty Registry
	register(h.agentRegistry)                 // populate in place
	return h
}

// newSnapshotHarness shares one InMemoryBlobs between the engine and every fake
// the factory mints, so a snapshot Put on the first run is readable by the
// fresh fake on resume (Slice 7.1). The caller overrides h.factory to program
// the fake / inject faults, re-wiring WithBlobs(h.blobs).
func newSnapshotHarness(t *testing.T, workflowYAML string) *harness {
	t.Helper()
	blobs := state.NewInMemoryBlobs()
	h := newHarness(t, func() container.Backend { return container.NewFake().WithBlobs(blobs) }, workflowYAML)
	h.blobs = blobs
	return h
}

func (h *harness) runWorkflow(t *testing.T) (engine.Outcome, error) {
	t.Helper()
	return h.runOrResume(t, false)
}

func (h *harness) resumeWorkflow(t *testing.T) (engine.Outcome, error) {
	t.Helper()
	return h.runOrResume(t, true)
}

func (h *harness) runOrResume(t *testing.T, isResume bool) (engine.Outcome, error) {
	t.Helper()

	ld, err := loader.Load(h.wfPath)
	if err != nil {
		return "", err
	}
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		t.Fatalf("harness: workflow invalid: %v", diags)
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		return "", err
	}

	var rs *engine.RunState
	var recordedAssets map[string]engine.RunStartedAsset
	if isResume {
		events, ferr := h.log.Fold()
		if ferr != nil {
			return "", ferr
		}
		foldedRS, ferr := engine.Fold(events, h.blobs)
		if ferr != nil {
			return "", ferr
		}
		if foldedRS.WorkflowDigest != digest {
			return "", &digestMismatchError{
				original: foldedRS.WorkflowDigest,
				current:  digest,
			}
		}
		rs = foldedRS
		recordedAssets = foldedRS.Assets
		if err := h.log.Reopen(); err != nil {
			return "", err
		}
		rs.Epoch++
		resumedData, _ := json.Marshal(engine.RunResumedData{Epoch: rs.Epoch})
		if err := h.log.Append(state.Event{
			Type: engine.EventRunResumed, Data: resumedData,
		}); err != nil {
			return "", err
		}
	} else {
		rs = engine.NewRunState(h.runID, digest, h.input)
		// If the harness has a pre-bound input, persist it as a blob and
		// record the InputRef in run.started so resume's Fold can restore
		// RunState.Input (engine/fold.go reads d.InputRef and re-materializes
		// via Blobs.Get). Without this, a resume after Bucket 7 map fixtures
		// would see rs.Input == nil and fail to evaluate `over: "{{ input.items }}"`.
		var inputRef string
		if h.input != nil {
			raw, mErr := json.Marshal(h.input)
			if mErr != nil {
				return "", mErr
			}
			ref, pErr := h.blobs.Put(raw)
			if pErr != nil {
				return "", pErr
			}
			inputRef = ref
		}
		assetSnapshots, err := engine.StoreRunStartedAssetsForLoadedDefinition(h.blobs, ld)
		if err != nil {
			return "", err
		}
		recordedAssets = assetSnapshots
		runStartedData, _ := json.Marshal(engine.RunStartedData{
			RunID: h.runID, WorkflowDigest: digest, InputRef: inputRef,
			Assets:   assetSnapshots,
			Runtimes: h.runtimes, // nil for non-role buckets (omitempty → byte-identical)
		})
		if err := h.log.Append(state.Event{
			Type: engine.EventRunStarted, Data: runStartedData,
		}); err != nil {
			return "", err
		}
	}

	backend := h.factory()
	ctx := context.Background()
	handles := make(map[string]container.Handle, len(ld.Workflow.Containers))
	defer func() {
		for _, hndl := range handles {
			_ = backend.Destroy(ctx, hndl)
		}
	}()
	for name, c := range ld.Workflow.Containers {
		// Slice 7.1 capability guard (mirrors cli/snapshotguard.go): a
		// snapshot:workspace container needs a snapshot-capable backend.
		if c.Snapshot == "workspace" && backend.Capabilities().Snapshot == container.SnapshotNone {
			return "", fmt.Errorf("harness: container %q declares snapshot: workspace but the backend cannot snapshot", name)
		}
		var hndl container.Handle
		var err error
		// Slice 7.1: on resume, restore a snapshot:workspace container from its
		// latest committed snapshot (folded into rs.SnapshotRefs) instead of
		// Create-ing a fresh one. No ref (crashed before first commit) → Create.
		if isResume && c.Snapshot == "workspace" && rs.SnapshotRefs[name] != "" {
			hndl, err = backend.Restore(ctx, container.SnapshotRef(rs.SnapshotRefs[name]), name)
		} else {
			hndl, err = backend.Create(ctx, container.ContainerSpec{Name: name})
		}
		if err != nil {
			return "", err
		}
		handles[name] = hndl
	}

	dispatcher := &engine.LocalDispatcher{
		Backend:      backend,
		Handles:      handles,
		ComposeFiles: ld.ComposeFiles,
		Resolver:     h.agentRegistry, // empty Registry by default (newHarness init); newHarnessWithAgentRegistry populates it
		// AgentEventTap: nil — conformance is silent; bucket tests assert log entries, not tap output
	}
	outcome, runErr := engine.Run(ctx, ld, rs, dispatcher, h.log, h.blobs, h.clk, engine.RunOptions{
		Broker: h.broker,
		Assets: recordedAssets,
		Resume: isResume,
	})
	return outcome, runErr
}

type digestMismatchError struct {
	original string
	current  string
}

func (e *digestMismatchError) Error() string {
	return "harness: workflow digest mismatch (original=" + e.original + ", current=" + e.current + ")"
}

type execProgram struct {
	cmd string
	res container.ExecResult
}

// preProgramFake wraps factory so every *container.Fake it returns is
// pre-programmed. Non-fake backends pass through unchanged.
func preProgramFake(t *testing.T, factory BackendFactory, programs []execProgram) BackendFactory {
	t.Helper()
	return func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		for _, p := range programs {
			fake.ProgramExec(p.cmd, p.res, nil)
		}
		return fake
	}
}

func mustFoldEvents(t *testing.T, h *harness) []state.Event {
	t.Helper()
	events, err := h.log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	return events
}
