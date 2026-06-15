package native

import (
	"archive/tar"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/state"
)

func TestTarHeaderZeroesOwnerAndTime(t *testing.T) {
	h := tarHeader("a.txt", tar.TypeReg, 0o644, 3, "")
	if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
		t.Errorf("owner leak: uid=%d gid=%d uname=%q gname=%q", h.Uid, h.Gid, h.Uname, h.Gname)
	}
	if !h.ModTime.IsZero() || !h.AccessTime.IsZero() || !h.ChangeTime.IsZero() {
		t.Errorf("time leak: mod=%v acc=%v chg=%v", h.ModTime, h.AccessTime, h.ChangeTime)
	}
}

func TestTarHeaderPreservesExecMasksSpecial(t *testing.T) {
	if got := tarHeader("x", tar.TypeReg, 0o755, 0, "").Mode; got != 0o755 {
		t.Errorf("exec mode = %o, want 0755", got)
	}
	mode := fs.FileMode(0o755) | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	if got := tarHeader("x", tar.TypeReg, mode, 0, "").Mode; got != 0o755 {
		t.Errorf("special-bit mode = %o, want 0755 (special bits masked)", got)
	}
}

func TestSnapshotWithoutBlobsErrors(t *testing.T) {
	b, _ := New(t.TempDir())
	h, _ := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	if _, err := b.Snapshot(context.Background(), h); err == nil {
		t.Fatal("Snapshot without blobs: err = nil, want non-nil")
	}
}

func TestSnapshotDeterministicRef(t *testing.T) {
	mk := func() container.SnapshotRef {
		root := t.TempDir()
		b, _ := New(root, WithBlobs(state.NewInMemoryBlobs()))
		h, _ := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
		wd := filepath.Join(root, "ws")
		if err := os.WriteFile(filepath.Join(wd, "a.txt"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		ref, err := b.Snapshot(context.Background(), h)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		return ref
	}
	if a, b2 := mk(), mk(); a != b2 || a == "" {
		t.Errorf("non-deterministic or empty SnapshotRef: %q vs %q", a, b2)
	}
}
