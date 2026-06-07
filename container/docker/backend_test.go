package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/client"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/state"
)

func TestNewRequiresClient(t *testing.T) {
	_, err := New(nil, "run-abc", state.NewInMemoryBlobs())
	if err == nil || !strings.Contains(err.Error(), "cli is required") {
		t.Errorf("New(nil, ...): err = %v", err)
	}
}

func TestNewRequiresRunID(t *testing.T) {
	cli := &client.Client{} // zero-value is fine — we never call into it.
	_, err := New(cli, "", state.NewInMemoryBlobs())
	if err == nil || !strings.Contains(err.Error(), "runID is required") {
		t.Errorf("New(_, \"\"): err = %v", err)
	}
}

func TestNewRejectsNilBlobs(t *testing.T) {
	_, err := New(&client.Client{}, "run-abc", nil)
	if err == nil {
		t.Fatal("New with nil blobs: err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "blobs is required") {
		t.Errorf("err = %q, want to contain \"blobs is required\"", err)
	}
}

func TestWithSnapshotMaxBlobBytesRejectsZeroAndNegative(t *testing.T) {
	for _, n := range []int64{0, -1, -1024} {
		_, err := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs(), WithSnapshotMaxBlobBytes(n))
		if err == nil {
			t.Errorf("New with WithSnapshotMaxBlobBytes(%d): err = nil, want non-nil", n)
			continue
		}
		if !strings.Contains(err.Error(), "WithSnapshotMaxBlobBytes") {
			t.Errorf("WithSnapshotMaxBlobBytes(%d): err = %q, want to mention \"WithSnapshotMaxBlobBytes\"", n, err)
		}
	}
}

func TestNewSucceeds(t *testing.T) {
	cli := &client.Client{}
	b, err := New(cli, "run-abc", state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil {
		t.Fatal("New returned nil Backend")
	}
}

func TestCapabilitiesAdvertisesFSCoW(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	if got := b.Capabilities().Snapshot; got != container.SnapshotFSCoW {
		t.Errorf("Capabilities().Snapshot = %q, want %q", got, container.SnapshotFSCoW)
	}
}

func TestExecHonorsCtxCancelWithoutDaemon(t *testing.T) {
	// Construct a Backend with a real-shaped client.Client (zero value is
	// fine — we never reach a daemon call because ctx is pre-cancelled).
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	// Register a fake handle so the implementation doesn't reject on
	// unknown-handle before checking ctx.
	b.mu.Lock()
	b.handles["fake-handle"] = registeredContainer{kind: kindImage, dockerID: "fake-handle"}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := b.Exec(ctx, container.Handle{Name: "lab", ID: "fake-handle"}, container.Cmd{Run: "/bin/true"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Exec with pre-cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestExecReturnsErrorOnUnknownHandle(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	_, _, err := b.Exec(context.Background(), container.Handle{Name: "lab", ID: "never-created"}, container.Cmd{Run: "/bin/true"})
	if err == nil {
		t.Fatal("Exec on unknown handle: err = nil, want non-nil")
	}
	// Don't assert specific wording; just confirm a non-nil error.
}

func TestCaptureFilesHonorsCtxCancelWithoutDaemon(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	b.mu.Lock()
	b.handles["fake-handle"] = registeredContainer{kind: kindImage, dockerID: "fake-handle"}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.CaptureFiles(ctx, container.Handle{Name: "lab", ID: "fake-handle"}, []string{"/etc/hosts"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CaptureFiles with pre-cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestCaptureFilesReturnsErrorOnUnknownHandle(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	_, err := b.CaptureFiles(context.Background(), container.Handle{Name: "lab", ID: "never-created"}, []string{"/etc/hosts"})
	if err == nil {
		t.Fatal("CaptureFiles on unknown handle: err = nil, want non-nil")
	}
}

func TestCaptureFilesEmptyPathsReturnsEmpty(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	b.mu.Lock()
	b.handles["fake-handle"] = registeredContainer{kind: kindImage, dockerID: "fake-handle"}
	b.mu.Unlock()

	out, err := b.CaptureFiles(context.Background(), container.Handle{Name: "lab", ID: "fake-handle"}, nil)
	if err != nil {
		t.Errorf("CaptureFiles with nil paths: err = %v, want nil", err)
	}
	if len(out) != 0 {
		t.Errorf("CaptureFiles with nil paths: out = %v, want []", out)
	}
}

func TestSnapshotHonorsCtxCancelWithoutDaemon(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	b.mu.Lock()
	b.handles["fake-handle"] = registeredContainer{kind: kindImage, dockerID: "fake-handle"}
	b.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Snapshot(ctx, container.Handle{Name: "lab", ID: "fake-handle"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Snapshot with pre-cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestSnapshotErrorsOnUnknownHandle(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	_, err := b.Snapshot(context.Background(), container.Handle{Name: "lab", ID: "never-created"})
	if err == nil {
		t.Fatal("Snapshot on unknown handle: err = nil, want non-nil")
	}
}

func TestSnapshotErrorsOnComposeHandle(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	b.mu.Lock()
	b.handles["awf-run-abc"] = registeredContainer{kind: kindCompose, project: "awf-run-abc", defaultSvc: "web"}
	b.mu.Unlock()
	_, err := b.Snapshot(context.Background(), container.Handle{Name: "lab", ID: "awf-run-abc", Service: "web"})
	if err == nil {
		t.Fatal("Snapshot on compose handle: err = nil, want non-nil")
	}
}

func TestRestoreHonorsCtxCancelWithoutDaemon(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Restore(ctx, container.SnapshotRef("awf-d1:sha256:abc@alpine@e30="), "lab")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Restore with pre-cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestRestoreErrorsOnMalformedRef(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	_, err := b.Restore(context.Background(), container.SnapshotRef("not-a-valid-ref"), "lab")
	if err == nil {
		t.Fatal("Restore on malformed ref: err = nil, want non-nil")
	}
}

func TestRestoreErrorsOnEmptyName(t *testing.T) {
	b, _ := New(&client.Client{}, "run-abc", state.NewInMemoryBlobs())
	_, err := b.Restore(context.Background(), container.SnapshotRef("awf-d1:sha256:abc@alpine@e30="), "")
	if err == nil {
		t.Fatal("Restore with empty name: err = nil, want non-nil")
	}
}

// parseMemBytes must accept the k8s-style binary suffix the man page documents
// (mem: 4Gi) as well as go-units' native forms — go-units RAMInBytes alone rejects
// the trailing "i" ("invalid suffix: 'gi'"), which silently broke every map
// per-item Create (swallowed as image_unavailable).
func TestParseMemBytes(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	const mib = int64(1024 * 1024)
	cases := map[string]int64{
		"4Gi":   4 * gib, // documented k8s-style binary
		"512Mi": 512 * mib,
		"4g":    4 * gib, // go-units native binary
		"4096":  4096,    // bare bytes
	}
	for in, want := range cases {
		got, err := parseMemBytes(in)
		if err != nil {
			t.Errorf("parseMemBytes(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseMemBytes(%q) = %d, want %d", in, got, want)
		}
	}
}
