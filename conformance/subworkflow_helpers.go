package conformance

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

func seedSubworkflowHalfCommit(t *testing.T, h *harness, callPath string, callInput map[string]any, childOutputs map[string]any) {
	t.Helper()
	ld := loadSubworkflowDefinition(t, h)
	digest, err := ld.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	assets, err := engine.StoreRunStartedAssetsForLoadedDefinition(h.blobs, ld)
	if err != nil {
		t.Fatalf("store run-started assets: %v", err)
	}
	appendEvent(t, h.log, state.Event{
		Type: engine.EventRunStarted,
		Data: mustJSON(t, engine.RunStartedData{RunID: h.runID, WorkflowDigest: digest, Assets: assets}),
	})
	inputRaw, err := json.Marshal(callInput)
	if err != nil {
		t.Fatalf("marshal call input: %v", err)
	}
	inputRef, err := h.blobs.Put(inputRaw)
	if err != nil {
		t.Fatalf("put call input: %v", err)
	}
	appendEvent(t, h.log, state.Event{
		Type: engine.EventCallStarted,
		Path: callPath,
		Data: mustJSON(t, engine.CallStartedData{InputRef: inputRef}),
	})
	outputRaw, err := json.Marshal(childOutputs)
	if err != nil {
		t.Fatalf("marshal child outputs: %v", err)
	}
	outputRef, err := h.blobs.Put(outputRaw)
	if err != nil {
		t.Fatalf("put child outputs: %v", err)
	}
	appendEvent(t, h.log, state.Event{
		Type: engine.EventNodeCompleted,
		Path: callPath + ".workflow.final",
		Data: mustJSON(t, engine.NodeCompletedData{Outcome: string(engine.OutcomeOK), OutputsRef: outputRef}),
	})
}

func loadSubworkflowDefinition(t *testing.T, h *harness) *ir.LoadedDefinition {
	t.Helper()
	ld, err := loader.Load(h.wfPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if diags := ir.Validate(ld); ir.HasErrors(diags) {
		t.Fatalf("workflow invalid: %v", diags)
	}
	return ld
}

func runLoadedSubworkflowResume(t *testing.T, h *harness, ld *ir.LoadedDefinition) (engine.Outcome, error) {
	t.Helper()
	events, err := h.log.Fold()
	if err != nil {
		return "", err
	}
	rs, err := engine.Fold(events, h.blobs)
	if err != nil {
		return "", err
	}
	if err := h.log.Reopen(); err != nil {
		return "", err
	}
	rs.Epoch++
	appendEvent(t, h.log, state.Event{
		Type: engine.EventRunResumed,
		Data: mustJSON(t, engine.RunResumedData{Epoch: rs.Epoch}),
	})

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
	dispatcher := &engine.LocalDispatcher{
		Backend:      backend,
		Handles:      handles,
		ComposeFiles: ld.ComposeFiles,
		Resolver:     h.agentRegistry,
	}
	return engine.Run(ctx, ld, rs, dispatcher, h.log, h.blobs, h.clk, engine.RunOptions{
		Assets: rs.Assets,
	})
}

func failingChildFactory(t *testing.T, factory BackendFactory) BackendFactory {
	t.Helper()
	first := true
	return func() container.Backend {
		f := factory().(*container.Fake)
		f.ProgramExec("./child.sh", container.ExecResult{ExitCode: 0}, nil)
		if first {
			f.FailExecAfterN(0)
			first = false
		}
		return f
	}
}

func appendEvent(t *testing.T, log state.Log, event state.Event) {
	t.Helper()
	if err := log.Append(event); err != nil {
		t.Fatalf("append %s at %q: %v", event.Type, event.Path, err)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return b
}

func writeSubworkflowFile(t *testing.T, h *harness, rel string, body string) {
	t.Helper()
	writeAssetFile(t, filepath.Join(h.baseDir, rel), []byte(body))
}

func sawExec(fake *container.Fake, run string) bool {
	for _, c := range fake.Calls {
		if c.Run == run {
			return true
		}
	}
	return false
}
