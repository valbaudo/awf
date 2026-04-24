//go:build integ

package docker

import (
	"bytes"
	"context"
	"errors"
	"testing"

	cont "github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/state"
)

func TestBucket11a_WorkspaceMutationRoundTrip(t *testing.T) {
	cli, b := newTestBackend(t, "bucket11a-roundtrip")
	h := newAlpineContainer(t, cli, b)
	ctx := context.Background()

	if _, _, err := b.Exec(ctx, h, cont.Cmd{Run: "mkdir -p /work && printf 'hello\\n' > /work/a.txt"}); err != nil {
		t.Fatalf("Exec setup: %v", err)
	}
	ref, err := b.Snapshot(ctx, h)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := b.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	h2, err := b.Restore(ctx, ref, "restored-lab")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h2) })
	if h2.Name != "restored-lab" {
		t.Errorf("Restore Handle.Name = %q, want \"restored-lab\"", h2.Name)
	}

	files, err := b.CaptureFiles(ctx, h2, []string{"/work/a.txt"})
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if len(files) != 1 || !bytes.Equal(files[0].Content, []byte("hello\n")) {
		t.Errorf("Content = %q, want \"hello\\n\"", files[0].Content)
	}
}

func TestBucket11b_DeletedFileRestoredAsDeleted(t *testing.T) {
	cli, b := newTestBackend(t, "bucket11b-delete")
	h := newAlpineContainer(t, cli, b)
	ctx := context.Background()

	// /etc/os-release ships in the alpine image (NOT a daemon bind-mount
	// like /etc/hostname).
	if _, _, err := b.Exec(ctx, h, cont.Cmd{Run: "rm /etc/os-release && mkdir -p /work && echo new > /work/x.txt"}); err != nil {
		t.Fatalf("Exec setup: %v", err)
	}
	ref, err := b.Snapshot(ctx, h)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := b.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	h2, err := b.Restore(ctx, ref, "restored-lab")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h2) })

	if files, err := b.CaptureFiles(ctx, h2, []string{"/work/x.txt"}); err != nil {
		t.Errorf("CaptureFiles /work/x.txt: %v", err)
	} else if len(files) != 1 || !bytes.Equal(files[0].Content, []byte("new\n")) {
		t.Errorf("added file content = %q, want \"new\\n\"", files[0].Content)
	}
	if _, err := b.CaptureFiles(ctx, h2, []string{"/etc/os-release"}); err == nil {
		t.Errorf("CaptureFiles /etc/os-release post-Restore: err = nil, want missing-path error")
	}
}

// TestBucket11c_OversizeDiffReturnsTypedError — Backend with
// WithSnapshotMaxBlobBytes(1024); a 64 KiB random workspace trips the
// cap with a ~60x margin (random data doesn't compress; gzip ratio
// ~1.001 + tar overhead). Bigger margin = no flakiness from
// gzip-flush timing edge cases.
func TestBucket11c_OversizeDiffReturnsTypedError(t *testing.T) {
	cli, err := newDockerClient()
	if err != nil {
		t.Fatalf("newDockerClient: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	blobs := state.NewInMemoryBlobs()
	b, err := New(cli, "test-bucket11c-oversize", blobs, WithSnapshotMaxBlobBytes(1024))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { cleanupOrphans(t, cli, containerPrefix(b.runID)) })

	h := newAlpineContainer(t, cli, b)
	ctx := context.Background()

	if _, _, err := b.Exec(ctx, h, cont.Cmd{Run: "mkdir -p /work && head -c 65536 /dev/urandom > /work/big.bin"}); err != nil {
		t.Fatalf("Exec setup: %v", err)
	}

	_, err = b.Snapshot(ctx, h)
	if err == nil {
		t.Fatal("Snapshot with 64 KiB random workspace, 1 KiB cap: err = nil, want *ErrSnapshotTooLarge")
	}
	var typed *ErrSnapshotTooLarge
	if !errors.As(err, &typed) {
		t.Fatalf("Snapshot err = %v, want errors.As(_, *ErrSnapshotTooLarge)", err)
	}
	if typed.Limit != 1024 {
		t.Errorf("ErrSnapshotTooLarge.Limit = %d, want 1024", typed.Limit)
	}
}
