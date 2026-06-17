package native

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
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

func TestSnapshotWithoutBlobsReturnsErrUnsupported(t *testing.T) {
	b, _ := New(t.TempDir())
	h, _ := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	_, err := b.Snapshot(context.Background(), h)
	if !errors.Is(err, container.ErrUnsupported) {
		t.Fatalf("Snapshot without blobs: err = %v, want errors.Is(_, container.ErrUnsupported)", err)
	}
}

func TestRestoreWithoutBlobsReturnsErrUnsupported(t *testing.T) {
	b, _ := New(t.TempDir())
	_, err := b.Restore(context.Background(), container.SnapshotRef("any"), "ws")
	if !errors.Is(err, container.ErrUnsupported) {
		t.Fatalf("Restore without blobs: err = %v, want errors.Is(_, container.ErrUnsupported)", err)
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

// buildBlobWithFiles builds a gzip-tar with one regular entry per size (names
// f0,f1,...), zero-filled bodies. Raw tar.Header so the test owns the bytes.
func buildBlobWithFiles(t *testing.T, blobs state.Blobs, sizes []int) container.SnapshotRef {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for i, sz := range sizes {
		if err := tw.WriteHeader(&tar.Header{
			Name:     fmt.Sprintf("f%d", i),
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(sz),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(make([]byte, sz)); err != nil {
			t.Fatal(err)
		}
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

// buildBlobWithNZeroLenEntries builds a gzip-tar with n zero-length regular
// entries (names e0..e{n-1}) — exercises the entry-count cap without any bytes.
func buildBlobWithNZeroLenEntries(t *testing.T, blobs state.Blobs, n int) container.SnapshotRef {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for i := 0; i < n; i++ {
		if err := tw.WriteHeader(&tar.Header{
			Name:     fmt.Sprintf("e%d", i),
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     0,
		}); err != nil {
			t.Fatal(err)
		}
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

// buildBlobWithLyingSize builds a single regular entry. archive/tar enforces
// Size on Write (you cannot write fewer bytes than declared and Close cleanly),
// so a truly forged "Size large, body small" tar cannot be constructed with the
// stdlib writer. We therefore build a LEGIT entry (Size == len(body)) and rely
// on the cumulative decompressed-byte budget — which counts bytes READ from the
// decompressor, never trusting hdr.Size — to govern. declaredSize must equal
// len(body); it is asserted so the contract (cap counts real bytes) is explicit.
func buildBlobWithLyingSize(t *testing.T, blobs state.Blobs, name string, declaredSize int64, body []byte) container.SnapshotRef {
	t.Helper()
	if declaredSize != int64(len(body)) {
		t.Fatalf("buildBlobWithLyingSize: archive/tar enforces Size==len(body); got declaredSize=%d len(body)=%d", declaredSize, len(body))
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     declaredSize,
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

func TestRestoreTripsCumulativeByteCap(t *testing.T) {
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	ref := buildBlobWithFiles(t, blobs, []int{400 << 10, 400 << 10, 400 << 10}) // 3×0.4 MiB
	b, _ := New(root, WithBlobs(blobs), WithSnapshotMaxRestoreBytes(1<<20))     // 1 MiB cap → 1.2 MiB total trips
	_, err := b.Restore(context.Background(), ref, "ws")
	if !errors.Is(err, container.ErrSnapshotTooLarge) {
		t.Fatalf("err = %v, want ErrSnapshotTooLarge", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "ws")); statErr == nil {
		t.Error("partial workdir not cleaned up after cap trip")
	}
}

func TestRestoreTripsEntryCountCap(t *testing.T) {
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	ref := buildBlobWithNZeroLenEntries(t, blobs, 10)
	b, _ := New(root, WithBlobs(blobs))
	b.maxEntries = 5 // test hook
	_, err := b.Restore(context.Background(), ref, "ws")
	if !errors.Is(err, container.ErrSnapshotTooLarge) {
		t.Fatalf("err = %v, want ErrSnapshotTooLarge", err)
	}
}

// TestRestoreLargeFileGovernedByCumulativeBudget locks the load-bearing
// contract: a single LEGIT large file (hdr.Size honest) is capped by the
// cumulative decompressed-byte budget, proving the cap counts bytes read from
// the decompressor rather than trusting any size figure. See
// buildBlobWithLyingSize for why a forged-Size tar is not constructible here.
func TestRestoreLargeFileGovernedByCumulativeBudget(t *testing.T) {
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	body := make([]byte, 2<<20) // 2 MiB
	ref := buildBlobWithLyingSize(t, blobs, "big", int64(len(body)), body)
	b, _ := New(root, WithBlobs(blobs), WithSnapshotMaxRestoreBytes(1<<20)) // 1 MiB cap → trips
	_, err := b.Restore(context.Background(), ref, "ws")
	if !errors.Is(err, container.ErrSnapshotTooLarge) {
		t.Fatalf("err = %v, want ErrSnapshotTooLarge", err)
	}
}

func TestSnapshotTripsCompressedCap(t *testing.T) {
	root := t.TempDir()
	b, _ := New(root, WithBlobs(state.NewInMemoryBlobs()), WithSnapshotMaxBlobBytes(1))
	h, _ := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	_ = os.WriteFile(filepath.Join(root, "ws", "big.txt"), make([]byte, 64<<10), 0o644)
	if _, err := b.Snapshot(context.Background(), h); !errors.Is(err, container.ErrSnapshotTooLarge) {
		t.Fatalf("Snapshot over compressed cap: err = %v, want ErrSnapshotTooLarge", err)
	}
}

func TestRestoreHugeCapDoesNotTruncate(t *testing.T) {
	root := t.TempDir()
	blobs := state.NewInMemoryBlobs()
	b, _ := New(root, WithBlobs(blobs), WithSnapshotMaxRestoreBytes(math.MaxInt64))
	h, _ := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	want := []byte("real content that must survive\n")
	if err := os.WriteFile(filepath.Join(root, "ws", "a.txt"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	ref, err := b.Snapshot(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Destroy(context.Background(), h)
	if _, err := b.Restore(context.Background(), ref, "ws"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "ws", "a.txt"))
	if err != nil || string(got) != string(want) {
		t.Errorf("restored content = %q (err %v), want %q — per-file CopyN+1 overflow truncated the file", got, err, want)
	}
}
