package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/client"

	"github.com/valbaudo/awf/container"
)

func TestNewRequiresClient(t *testing.T) {
	_, err := New(nil, "run-abc")
	if err == nil || !strings.Contains(err.Error(), "cli is required") {
		t.Errorf("New(nil, ...): err = %v", err)
	}
}

func TestNewRequiresRunID(t *testing.T) {
	cli := &client.Client{} // zero-value is fine — we never call into it.
	_, err := New(cli, "")
	if err == nil || !strings.Contains(err.Error(), "runID is required") {
		t.Errorf("New(_, \"\"): err = %v", err)
	}
}

func TestNewSucceeds(t *testing.T) {
	cli := &client.Client{}
	b, err := New(cli, "run-abc")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil {
		t.Fatal("New returned nil Backend")
	}
}

func TestCapabilitiesAdvertisesFSCoW(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc")
	if got := b.Capabilities().Snapshot; got != container.SnapshotFSCoW {
		t.Errorf("Capabilities().Snapshot = %q, want %q", got, container.SnapshotFSCoW)
	}
}

func TestExecHonorsCtxCancelWithoutDaemon(t *testing.T) {
	// Construct a Backend with a real-shaped client.Client (zero value is
	// fine — we never reach a daemon call because ctx is pre-cancelled).
	b, _ := New(&client.Client{}, "run-abc")
	// Register a fake handle so the implementation doesn't reject on
	// unknown-handle before checking ctx.
	b.mu.Lock()
	b.handles["fake-handle"] = "fake-handle"
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := b.Exec(ctx, container.Handle{Name: "lab", ID: "fake-handle"}, container.Cmd{Run: "/bin/true"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Exec with pre-cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestExecReturnsErrorOnUnknownHandle(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc")
	_, _, err := b.Exec(context.Background(), container.Handle{Name: "lab", ID: "never-created"}, container.Cmd{Run: "/bin/true"})
	if err == nil {
		t.Fatal("Exec on unknown handle: err = nil, want non-nil")
	}
	// Don't assert specific wording; just confirm a non-nil error.
}

func TestCaptureFilesHonorsCtxCancelWithoutDaemon(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc")
	b.mu.Lock()
	b.handles["fake-handle"] = "fake-handle"
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.CaptureFiles(ctx, container.Handle{Name: "lab", ID: "fake-handle"}, []string{"/etc/hosts"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CaptureFiles with pre-cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestCaptureFilesReturnsErrorOnUnknownHandle(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc")
	_, err := b.CaptureFiles(context.Background(), container.Handle{Name: "lab", ID: "never-created"}, []string{"/etc/hosts"})
	if err == nil {
		t.Fatal("CaptureFiles on unknown handle: err = nil, want non-nil")
	}
}

func TestCaptureFilesEmptyPathsReturnsEmpty(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc")
	b.mu.Lock()
	b.handles["fake-handle"] = "fake-handle"
	b.mu.Unlock()

	out, err := b.CaptureFiles(context.Background(), container.Handle{Name: "lab", ID: "fake-handle"}, nil)
	if err != nil {
		t.Errorf("CaptureFiles with nil paths: err = %v, want nil", err)
	}
	if len(out) != 0 {
		t.Errorf("CaptureFiles with nil paths: out = %v, want []", out)
	}
}

func TestSnapshotReturnsNotImplemented(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc")
	_, err := b.Snapshot(context.Background(), container.Handle{})
	var stubErr *ErrNotImplementedInSlice41
	if !errors.As(err, &stubErr) {
		t.Fatalf("Snapshot: err = %v, want *ErrNotImplementedInSlice41", err)
	}
	if stubErr.Method != "Snapshot" {
		t.Errorf("stubErr.Method = %q, want \"Snapshot\"", stubErr.Method)
	}
}

func TestRestoreReturnsNotImplemented(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc")
	_, err := b.Restore(context.Background(), container.SnapshotRef("any"))
	var stubErr *ErrNotImplementedInSlice41
	if !errors.As(err, &stubErr) {
		t.Fatalf("Restore: err = %v, want *ErrNotImplementedInSlice41", err)
	}
	if stubErr.Method != "Restore" {
		t.Errorf("stubErr.Method = %q, want \"Restore\"", stubErr.Method)
	}
}

func TestErrNotImplementedInSlice41Format(t *testing.T) {
	e := &ErrNotImplementedInSlice41{Method: "Snapshot"}
	if got := e.Error(); !strings.Contains(got, "Snapshot") || !strings.Contains(got, "slice 4.1") {
		t.Errorf("Error() = %q", got)
	}
}
