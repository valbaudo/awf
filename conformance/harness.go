package conformance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
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
type harness struct {
	wfPath  string
	clk     *clock.Fake
	log     *state.InMemoryLog
	blobs   *state.InMemoryBlobs
	factory BackendFactory
	runID   string
	input   map[string]any
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
		wfPath:  wfPath,
		clk:     clk,
		log:     state.NewInMemoryLog(clk),
		blobs:   state.NewInMemoryBlobs(),
		factory: factory,
		runID:   "conformance-run",
	}
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
	digest, err := ld.Workflow.ComputeDigest(ld.ComposeFiles)
	if err != nil {
		return "", err
	}

	var rs *engine.RunState
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
		runStartedData, _ := json.Marshal(engine.RunStartedData{
			RunID: h.runID, WorkflowDigest: digest, InputRef: inputRef,
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
	for name := range ld.Workflow.Containers {
		hndl, err := backend.Create(ctx, container.ContainerSpec{Name: name})
		if err != nil {
			return "", err
		}
		handles[name] = hndl
	}

	dispatcher := &engine.LocalDispatcher{Backend: backend, Handles: handles}
	outcome, runErr := engine.Run(ctx, ld, rs, dispatcher, h.log, h.blobs, h.clk, nil)
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
