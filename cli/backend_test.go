package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// ---- newBackend tests (daemon-free) ----

func TestNewBackendFakeKindReturnsFake(t *testing.T) {
	t.Parallel()
	backend, cleanup, err := cli.NewBackendForTest(context.Background(), engine.BackendFake, "run-abc", state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("newBackend(fake): %v", err)
	}
	defer cleanup()
	if backend == nil {
		t.Fatal("backend = nil")
	}
	if got := backend.Capabilities().Snapshot; got != container.SnapshotNone {
		t.Errorf("Capabilities().Snapshot = %v, want SnapshotNone (fake)", got)
	}
}

func TestNewBackendUnknownKindIsError(t *testing.T) {
	t.Parallel()
	_, _, err := cli.NewBackendForTest(context.Background(), "containerd", "run-abc", state.NewInMemoryBlobs())
	if err == nil {
		t.Fatal("err = nil, want non-nil (unknown kind)")
	}
	if !strings.Contains(err.Error(), "containerd") {
		t.Errorf("err = %q, want to mention the unknown kind", err)
	}
}

// (No TestNewBackendEmptyKindIsError — the default arm of newBackend's
// switch handles "" with the same "unknown backend kind" error path; an
// explicit empty-case test would exercise dead code per slice-4.5 plan
// §Major #8.)

// ---- readBackendKindFromLog tests (pure function over event slice) ----

func TestReadBackendKindFromLogReturnsRecorded(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(engine.RunStartedData{
		RunID:          "r1",
		WorkflowDigest: "sha256:x",
		Backend:        engine.BackendFake,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []state.Event{{Type: engine.EventRunStarted, Data: payload}}
	kind, err := cli.ReadBackendKindFromLogForTest(events)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if kind != engine.BackendFake {
		t.Errorf("kind = %q, want %q", kind, engine.BackendFake)
	}
}

func TestReadBackendKindFromLogDefaultsLegacyLogToDocker(t *testing.T) {
	t.Parallel()
	// Pre-slice-4.5 payload: no Backend field.
	payload, err := json.Marshal(engine.RunStartedData{
		RunID:          "r1",
		WorkflowDigest: "sha256:x",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []state.Event{{Type: engine.EventRunStarted, Data: payload}}
	kind, err := cli.ReadBackendKindFromLogForTest(events)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if kind != engine.BackendDocker {
		t.Errorf("kind = %q, want %q (legacy log default)", kind, engine.BackendDocker)
	}
}

func TestReadBackendKindFromLogRejectsUnknownKind(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(engine.RunStartedData{
		RunID:          "r1",
		WorkflowDigest: "sha256:x",
		Backend:        "containerd",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []state.Event{{Type: engine.EventRunStarted, Data: payload}}
	_, err = cli.ReadBackendKindFromLogForTest(events)
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "containerd") {
		t.Errorf("err = %q, want to mention the unknown kind", err)
	}
}

func TestReadBackendKindFromLogErrorsIfNoRunStarted(t *testing.T) {
	t.Parallel()
	// Defensive: a log without a run.started event is malformed by
	// construction, but we want a clean error rather than an empty string
	// silently defaulting to docker.
	events := []state.Event{}
	_, err := cli.ReadBackendKindFromLogForTest(events)
	if err == nil {
		t.Fatal("err = nil, want non-nil (no run.started event)")
	}
	if !strings.Contains(err.Error(), "run.started") {
		t.Errorf("err = %q, want to mention 'run.started'", err)
	}
}
