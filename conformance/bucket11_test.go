//go:build integ

package conformance

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/docker"
)

// testBucket11 runs the Phase 4 design §G "Bucket 11 — Snapshot/Restore"
// inventory. Migrated from container/docker/snapshot_integ_test.go
// (slice 4.4) in slice 4.6.
func testBucket11(t *testing.T, factory DockerBackendFactory) {
	t.Helper()
	t.Run("workspace_round_trip", func(t *testing.T) { testBucket11WorkspaceRoundTrip(t, factory) })
	t.Run("deleted_file_restored_as_deleted", func(t *testing.T) { testBucket11DeletedFileRestoredAsDeleted(t, factory) })
	t.Run("oversize_diff_typed_error", func(t *testing.T) { testBucket11OversizeDiffTypedError(t, factory) })
}

// testBucket11WorkspaceRoundTrip migrates TestBucket11a_WorkspaceMutationRoundTrip.
func testBucket11WorkspaceRoundTrip(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket11a-roundtrip")
	h := env.NewAlpineHandle(t, "lab")
	ctx := context.Background()

	if _, _, err := env.Backend.Exec(ctx, h, container.Cmd{
		Run: "mkdir -p /work && printf 'hello\\n' > /work/a.txt",
	}); err != nil {
		t.Fatalf("Exec setup: %v", err)
	}
	ref, err := env.Backend.Snapshot(ctx, h)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := env.Backend.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	h2, err := env.Backend.Restore(ctx, ref, "restored-lab")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = env.Backend.Destroy(ctx, h2) })

	if h2.Name != "restored-lab" {
		t.Errorf("Restore Handle.Name = %q, want \"restored-lab\"", h2.Name)
	}
	files, err := env.Backend.CaptureFiles(ctx, h2, []string{"/work/a.txt"})
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if len(files) != 1 || !bytes.Equal(files[0].Content, []byte("hello\n")) {
		t.Errorf("Content = %q, want \"hello\\n\"", files[0].Content)
	}
}

// testBucket11DeletedFileRestoredAsDeleted migrates
// TestBucket11b_DeletedFileRestoredAsDeleted. The diff-tar captures both
// added/modified paths AND a sidecar deletes-list; Restore must apply
// both.
func testBucket11DeletedFileRestoredAsDeleted(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket11b-delete")
	h := env.NewAlpineHandle(t, "lab")
	ctx := context.Background()

	// /etc/os-release ships in the alpine image (not a daemon bind-mount
	// like /etc/hostname). Deleting it produces a "delete" entry in
	// ContainerDiff that the snapshot tar must carry forward.
	if _, _, err := env.Backend.Exec(ctx, h, container.Cmd{
		Run: "rm /etc/os-release && mkdir -p /work && echo new > /work/x.txt",
	}); err != nil {
		t.Fatalf("Exec setup: %v", err)
	}
	ref, err := env.Backend.Snapshot(ctx, h)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := env.Backend.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	h2, err := env.Backend.Restore(ctx, ref, "restored-lab")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = env.Backend.Destroy(ctx, h2) })

	if files, err := env.Backend.CaptureFiles(ctx, h2, []string{"/work/x.txt"}); err != nil {
		t.Errorf("CaptureFiles /work/x.txt: %v", err)
	} else if len(files) != 1 || !bytes.Equal(files[0].Content, []byte("new\n")) {
		t.Errorf("added file content = %q, want \"new\\n\"", files[0].Content)
	}
	if _, err := env.Backend.CaptureFiles(ctx, h2, []string{"/etc/os-release"}); err == nil {
		t.Error("CaptureFiles /etc/os-release post-Restore: err = nil, want missing-path error")
	}
}

// testBucket11OversizeDiffTypedError migrates
// TestBucket11c_OversizeDiffReturnsTypedError. Uses
// docker.WithSnapshotMaxBlobBytes(1024) on the factory's opts; writes
// 64 KiB of random data (incompressible) into /work/big.bin, then expects
// Snapshot to return *docker.ErrSnapshotTooLarge.
func testBucket11OversizeDiffTypedError(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket11c-oversize", docker.WithSnapshotMaxBlobBytes(1024))
	h := env.NewAlpineHandle(t, "lab")
	ctx := context.Background()

	if _, _, err := env.Backend.Exec(ctx, h, container.Cmd{
		Run: "mkdir -p /work && head -c 65536 /dev/urandom > /work/big.bin",
	}); err != nil {
		t.Fatalf("Exec setup: %v", err)
	}
	_, err := env.Backend.Snapshot(ctx, h)
	if err == nil {
		t.Fatal("Snapshot with 64 KiB random workspace + 1 KiB cap: err = nil, want *docker.ErrSnapshotTooLarge")
	}
	var typed *docker.ErrSnapshotTooLarge
	if !errors.As(err, &typed) {
		t.Fatalf("Snapshot err = %v, want errors.As(_, *docker.ErrSnapshotTooLarge)", err)
	}
	if typed.Limit != 1024 {
		t.Errorf("ErrSnapshotTooLarge.Limit = %d, want 1024", typed.Limit)
	}
}
