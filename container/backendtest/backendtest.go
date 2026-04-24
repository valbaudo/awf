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
