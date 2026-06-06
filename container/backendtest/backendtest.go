// Package backendtest is the parameterized interface-conformance test for
// container.Backend. It exercises ONLY what every Backend impl is contractually
// obliged to do (Capabilities returns a known mode; Create→Destroy lifecycle;
// Snapshot/Restore route ErrUnsupported when Caps says "none"; double-Destroy
// errors per the io.Closer convention).
//
// Backend-specific behavior tests (scripted Exec for the fake, real shell
// execution for Docker, fault hooks for the fake) live in each backend's own
// *_test.go file. Phase 2 has one Backend — building a Factory + callback
// machinery here would be the speculative abstraction CLAUDE.md forbids
// ("no speculative interfaces, registries, or plugin layers"). When Phase 4's
// Docker impl exists, we'll learn which abstraction shape actually pays.
//
// Pattern: Go stdlib testing/fstest. Used both by Phase 2's fake (slice 2.2)
// and Phase 4's Docker impl unchanged.
package backendtest

import (
	"context"
	"errors"
	"testing"

	"github.com/valbaudo/awf/container"
)

// RunBasicContract verifies that b honors the container.Backend interface
// contract. Caller passes a configured Backend instance; the helper does the
// rest. Each sub-test takes b as-is — for tests that need a clean slate, the
// caller can re-create the backend before each RunBasicContract call.
func RunBasicContract(t *testing.T, b container.Backend) {
	t.Helper()
	t.Run("CapabilitiesReturnsKnownMode", func(t *testing.T) { testCapsKnownMode(t, b) })
	t.Run("CreateThenDestroy", func(t *testing.T) { testCreateThenDestroy(t, b) })
	t.Run("DoubleDestroyErrors", func(t *testing.T) { testDoubleDestroy(t, b) })
	t.Run("SnapshotErrUnsupportedIfNotAdvertised", func(t *testing.T) { testSnapshotRouting(t, b) })
	t.Run("RestoreErrUnsupportedIfNotAdvertised", func(t *testing.T) { testRestoreRouting(t, b) })
	t.Run("ExecHonorsContextCancel", func(t *testing.T) { testExecCtxCancel(t, b) })
	t.Run("CaptureFilesHonorsContextCancel", func(t *testing.T) { testCaptureFilesCtxCancel(t, b) })
}

// RunCopyToContract verifies CopyTo stages bytes that a subsequent CaptureFiles
// reads back (the artifact round-trip). Uses a NESTED destination path so the
// Docker impl exercises parent-directory creation. Caller passes a configured
// Backend; the helper Creates/Destroys its own container.
func RunCopyToContract(t *testing.T, b container.Backend) {
	t.Helper()
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "copyto"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = b.Destroy(ctx, h) }()

	want := []byte("hello-artifact\n")
	dst := "/work/sub/in.txt" // nested: ancestors must be created on real docker
	if err := b.CopyTo(ctx, h, []container.InputFile{{Path: dst, Content: want}}); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	got, err := b.CaptureFiles(ctx, h, []string{dst})
	if err != nil {
		t.Fatalf("CaptureFiles after CopyTo: %v", err)
	}
	if len(got) != 1 || string(got[0].Content) != string(want) {
		t.Errorf("round-trip mismatch: got %v, want %q", got, want)
	}
	if err := b.CopyTo(ctx, h, nil); err != nil {
		t.Errorf("CopyTo(nil): got %v, want nil", err)
	}

	// Cancelled context ⇒ non-nil error wrapping context.Canceled (contract,
	// mirrors Exec / CaptureFiles).
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := b.CopyTo(cancelCtx, h, []container.InputFile{{Path: "/work/c.txt", Content: []byte("x")}}); !errors.Is(err, context.Canceled) {
		t.Errorf("CopyTo with cancelled ctx: err = %v, want errors.Is(_, context.Canceled)", err)
	}

	// Unknown handle ⇒ hard error.
	if err := b.CopyTo(ctx, container.Handle{Name: "ghost", ID: "ghost-nonexistent"}, []container.InputFile{{Path: "/work/c.txt", Content: []byte("x")}}); err == nil {
		t.Errorf("CopyTo with unknown handle: got nil, want error")
	}
}

func testCapsKnownMode(t *testing.T, b container.Backend) {
	switch m := b.Capabilities().Snapshot; m {
	case container.SnapshotNone, container.SnapshotFSCoW:
		// ok
	default:
		t.Errorf("Capabilities().Snapshot = %q, want %q or %q",
			m, container.SnapshotNone, container.SnapshotFSCoW)
	}
}

func testCreateThenDestroy(t *testing.T, b container.Backend) {
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.Name != "lab" {
		t.Errorf("Handle.Name = %q, want \"lab\"", h.Name)
	}
	if h.ID == "" {
		t.Errorf("Handle.ID is empty (backend must assign one)")
	}
	if err := b.Destroy(ctx, h); err != nil {
		t.Errorf("Destroy: %v", err)
	}
}

func testDoubleDestroy(t *testing.T, b container.Backend) {
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "scratch"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Destroy(ctx, h); err != nil {
		t.Fatalf("first Destroy: %v", err)
	}
	// Second Destroy on the same handle must error — per os.File.Close / Docker ContainerRemove precedent.
	if err := b.Destroy(ctx, h); err == nil {
		t.Errorf("second Destroy returned nil; want error (os.File.Close convention)")
	}
}

func testSnapshotRouting(t *testing.T, b container.Backend) {
	if b.Capabilities().Snapshot != container.SnapshotNone {
		t.Skip("backend advertises snapshot support; ErrUnsupported routing N/A")
	}
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "workspace"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = b.Destroy(ctx, h) }()
	_, err = b.Snapshot(ctx, h)
	if !errors.Is(err, container.ErrUnsupported) {
		t.Errorf("Snapshot: err = %v, want errors.Is(_, ErrUnsupported)", err)
	}
}

func testRestoreRouting(t *testing.T, b container.Backend) {
	if b.Capabilities().Snapshot != container.SnapshotNone {
		t.Skip("backend advertises snapshot support; ErrUnsupported routing N/A")
	}
	_, err := b.Restore(context.Background(), container.SnapshotRef("any"), "test")
	if !errors.Is(err, container.ErrUnsupported) {
		t.Errorf("Restore: err = %v, want errors.Is(_, ErrUnsupported)", err)
	}
}

func testExecCtxCancel(t *testing.T, b container.Backend) {
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "ctx-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = b.Destroy(ctx, h) }()
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	// The cmd may not be programmed on this backend (e.g. Phase 2 fake requires
	// ProgramExec). Backends MAY route the cancellation check BEFORE the
	// unknown-cmd check; backends MAY also route it AFTER. The contract is
	// "cancelled ctx ⇒ non-nil error" — either path satisfies. We assert only
	// that the returned error wraps context.Canceled.
	_, _, err = b.Exec(cancelCtx, h, container.Cmd{Run: "/bin/true"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Exec with cancelled ctx: err = %v, want errors.Is(_, context.Canceled)", err)
	}
}

func testCaptureFilesCtxCancel(t *testing.T, b container.Backend) {
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: "ctx-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = b.Destroy(ctx, h) }()
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = b.CaptureFiles(cancelCtx, h, []string{"/nonexistent"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CaptureFiles with cancelled ctx: err = %v, want errors.Is(_, context.Canceled)", err)
	}
}

// RunSnapshotContract verifies that b honors the Snapshot+Restore round-trip
// contract for backends advertising SnapshotFSCoW. Skipped on SnapshotNone
// backends — their behavior is covered by RunBasicContract's
// testSnapshotRouting sub-test.
//
//   - image: the digest-pinned reference Create will use.
//   - name:  the IR-declared container name (passed to Restore per slice
//     4.4 Design Q9).
//
// Three sub-tests align with Phase 4 design decision 12 (Bucket 11):
//   - WorkspaceMutationCapturedAndRestored (11a)
//   - DeletedFileRestoredAsDeleted        (11b)
//   - SmallWorkspaceDoesNotTripDefaultCap (smoke; the real cap-trip
//     assertion is Docker integ TestBucket11c via WithSnapshotMaxBlobBytes).
func RunSnapshotContract(t *testing.T, b container.Backend, image, name string) {
	t.Helper()
	if b.Capabilities().Snapshot != container.SnapshotFSCoW {
		t.Skip("backend does not advertise SnapshotFSCoW; RunSnapshotContract N/A")
	}
	t.Run("WorkspaceMutationCapturedAndRestored", func(t *testing.T) { testSnapshotRoundTrip(t, b, image, name+"-a") })
	t.Run("DeletedFileRestoredAsDeleted", func(t *testing.T) { testSnapshotDeleteRestore(t, b, image, name+"-b") })
	t.Run("SmallWorkspaceDoesNotTripDefaultCap", func(t *testing.T) { testSnapshotSmallWorkspaceNoTrip(t, b, image, name+"-c") })
}

func testSnapshotRoundTrip(t *testing.T, b container.Backend, image, name string) {
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: name, Image: image})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	const wantPath = "/work/a.txt"
	const wantBody = "hello captured\n"
	chunks, resultCh, err := b.Exec(ctx, h, container.Cmd{Run: "mkdir -p /work && echo 'hello captured' > " + wantPath})
	if err != nil {
		t.Fatalf("Exec write: %v", err)
	}
	for range chunks {
	}
	<-resultCh

	ref, err := b.Snapshot(ctx, h)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if ref == "" {
		t.Fatal("Snapshot returned empty ref")
	}
	if err := b.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy (pre-Restore): %v", err)
	}

	h2, err := b.Restore(ctx, ref, name+"-restored")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h2) })
	if h2.Name != name+"-restored" {
		t.Errorf("Restore Handle.Name = %q, want %q", h2.Name, name+"-restored")
	}

	files, err := b.CaptureFiles(ctx, h2, []string{wantPath})
	if err != nil {
		t.Fatalf("CaptureFiles after Restore: %v", err)
	}
	if len(files) != 1 || string(files[0].Content) != wantBody {
		t.Errorf("restored content = %q, want %q", files[0].Content, wantBody)
	}
}

func testSnapshotDeleteRestore(t *testing.T, b container.Backend, image, name string) {
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: name, Image: image})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	// /etc/os-release is a real image-shipped file on alpine (NOT a daemon
	// bind-mount like /etc/hostname which wouldn't show in ContainerDiff).
	const addedPath = "/work/created.txt"
	chunks, resultCh, err := b.Exec(ctx, h, container.Cmd{Run: "mkdir -p /work && echo new > " + addedPath + " && rm /etc/os-release"})
	if err != nil {
		t.Fatalf("Exec setup: %v", err)
	}
	for range chunks {
	}
	<-resultCh
	ref, err := b.Snapshot(ctx, h)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := b.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	h2, err := b.Restore(ctx, ref, name+"-restored")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h2) })

	files, err := b.CaptureFiles(ctx, h2, []string{addedPath})
	if err != nil {
		t.Errorf("CaptureFiles added: %v", err)
	} else if len(files) != 1 || string(files[0].Content) != "new\n" {
		t.Errorf("added file post-Restore: got %+v, want \"new\\n\"", files)
	}
	if _, err := b.CaptureFiles(ctx, h2, []string{"/etc/os-release"}); err == nil {
		t.Errorf("CaptureFiles /etc/os-release post-Restore: err = nil, want missing-path error")
	}
}

func testSnapshotSmallWorkspaceNoTrip(t *testing.T, b container.Backend, image, name string) {
	ctx := context.Background()
	h, err := b.Create(ctx, container.ContainerSpec{Name: name, Image: image})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	chunks, resultCh, err := b.Exec(ctx, h, container.Cmd{Run: "mkdir -p /work && head -c 10240 /dev/zero > /work/small.bin"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range chunks {
	}
	<-resultCh
	if _, err := b.Snapshot(ctx, h); err != nil {
		t.Errorf("Snapshot of ~10 KiB workspace: %v (default cap should be vastly larger)", err)
	}
}
