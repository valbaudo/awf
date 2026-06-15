package native

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func TestRestoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	b, _ := New(root, WithBlobs(blobs))
	h, _ := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	wd := filepath.Join(root, "ws")
	_ = os.WriteFile(filepath.Join(wd, "a.txt"), []byte("hello\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wd, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	ref, err := b.Snapshot(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Destroy(context.Background(), h)

	_, err = b.Restore(context.Background(), ref, "ws")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "ws", "a.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Errorf("restored a.txt = %q, %v", got, err)
	}
	fi, _ := os.Stat(filepath.Join(root, "ws", "run.sh"))
	if fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("exec bit not preserved on restore: %v", fi)
	}
}

func TestRestoreRejectsBadNames(t *testing.T) {
	root := t.TempDir()
	b, _ := New(root, WithBlobs(state.NewInMemoryBlobs()))
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "keep")
	_ = os.WriteFile(victim, []byte("x"), 0o644)
	for _, name := range []string{"", "..", "../escape", "/abs", "a/../../b", "."} {
		if _, err := b.Restore(context.Background(), container.SnapshotRef("awf-d1:sha256:00"), name); err == nil {
			t.Errorf("Restore(name=%q): err = nil, want non-nil", name)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("victim outside root was disturbed: %v", err)
	}
}

func TestRestoreSymlinkTraversalConfined(t *testing.T) {
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	victimDir := t.TempDir()
	ref := buildMaliciousSymlinkBlob(t, blobs, victimDir)
	b, _ := New(root, WithBlobs(blobs))
	_, err := b.Restore(context.Background(), ref, "ws")
	if err == nil {
		t.Fatal("Restore of write-through-symlink blob: err = nil, want escape error")
	}
	if _, statErr := os.Stat(filepath.Join(victimDir, "x")); statErr == nil {
		t.Fatal("write-through symlink escaped os.Root: victim/x was created")
	}
}

// buildMaliciousSymlinkBlob builds, in memory, a gzip-tar that a naive extractor
// would use to write OUTSIDE the workdir: an absolute symlink "evil" -> victimDir
// followed by a regular file "evil/x" (so the file write traverses the symlink to
// victimDir/x). Raw tar.Header (NOT tarHeader) so the test controls the bytes.
func buildMaliciousSymlinkBlob(t *testing.T, blobs state.Blobs, victimDir string) container.SnapshotRef {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "evil",
		Typeflag: tar.TypeSymlink,
		Linkname: victimDir, // absolute path outside the root
		Mode:     0o777,
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte("pwned")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "evil/x",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	ref, err := blobs.Put(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return container.SnapshotRef(ref)
}
