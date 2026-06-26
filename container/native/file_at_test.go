package native_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

// newTestBackendAndHandle creates a Backend with a temp workdirRoot and a
// single container handle, returning both for use in file_at tests.
func newTestBackendAndHandle(t *testing.T) (*native.Backend, container.Handle) {
	t.Helper()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return b, h
}

// TestFileAtRoundTrip verifies that WriteFileAt followed by ReadFileAt returns
// the same bytes.
func TestFileAtRoundTrip(t *testing.T) {
	t.Parallel()
	b, h := newTestBackendAndHandle(t)
	ctx := context.Background()

	want := []byte("hello native file_at")
	if err := b.WriteFileAt(ctx, h, "hello.txt", want); err != nil {
		t.Fatalf("WriteFileAt: %v", err)
	}
	got, err := b.ReadFileAt(ctx, h, "hello.txt")
	if err != nil {
		t.Fatalf("ReadFileAt: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ReadFileAt = %q, want %q", got, want)
	}
}

// TestFileAtWriteCreatesParentDirs verifies that WriteFileAt creates
// intermediate directories so a nested path like "a/b/c.txt" works without
// the caller pre-creating "a/b/".
func TestFileAtWriteCreatesParentDirs(t *testing.T) {
	t.Parallel()
	b, h := newTestBackendAndHandle(t)
	ctx := context.Background()

	want := []byte("nested content")
	if err := b.WriteFileAt(ctx, h, "a/b/c.txt", want); err != nil {
		t.Fatalf("WriteFileAt nested: %v", err)
	}
	got, err := b.ReadFileAt(ctx, h, "a/b/c.txt")
	if err != nil {
		t.Fatalf("ReadFileAt nested: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ReadFileAt nested = %q, want %q", got, want)
	}
}

// TestFileAtPathEscapeRejected verifies that a path-escape attempt ("../escape"
// or an absolute path outside the workdir) is confined and does NOT write
// outside the workdir root. The implementation must either reject the path
// with an error or silently confine it — we assert that the sentinel file
// does NOT appear outside the workdir root.
func TestFileAtPathEscapeRejected(t *testing.T) {
	t.Parallel()
	// Use a fixed temp root so we can check a sibling dir.
	root := t.TempDir()
	b, err := native.New(root)
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ctx := context.Background()

	// "../escape" attempts to write one level above the workdir (still within
	// root, but in a sibling of "ws/"). The write should either fail or be
	// confined inside "ws/".
	_ = b.WriteFileAt(ctx, h, "../escape", []byte("escaped"))

	// The sentinel must NOT be present at root/escape (one level above ws/).
	escapedPath := filepath.Join(root, "escape")
	if _, statErr := os.Stat(escapedPath); statErr == nil {
		t.Errorf("path-escape succeeded: sentinel file found at %s", escapedPath)
	}

	// "../../deep-escape" climbs TWO levels above the workdir. The path-cleaning
	// collapses the leading "../" segments so it is confined inside "ws/"; the
	// sentinel must not appear two levels up (the parent of root).
	_ = b.WriteFileAt(ctx, h, "../../deep-escape", []byte("deep-escaped"))
	deepEscaped := filepath.Join(filepath.Dir(root), "deep-escape")
	if _, statErr := os.Stat(deepEscaped); statErr == nil {
		t.Errorf("two-level path-escape succeeded: sentinel file found at %s", deepEscaped)
	}

	// Also try an absolute path outside root entirely.
	absTarget := filepath.Join(t.TempDir(), "abs-escape")
	_ = b.WriteFileAt(ctx, h, absTarget, []byte("abs-escaped"))
	if _, statErr := os.Stat(absTarget); statErr == nil {
		t.Errorf("absolute-path escape succeeded: sentinel file found at %s", absTarget)
	}
}

// TestFileAtReadMissingIsHardError verifies that ReadFileAt on a path that
// does not exist returns a non-nil error.
func TestFileAtReadMissingIsHardError(t *testing.T) {
	t.Parallel()
	b, h := newTestBackendAndHandle(t)
	ctx := context.Background()

	_, err := b.ReadFileAt(ctx, h, "does-not-exist.txt")
	if err == nil {
		t.Error("ReadFileAt missing path: err = nil, want non-nil error")
	}
}

// TestFileAtUnknownHandleIsHardError verifies that both ReadFileAt and
// WriteFileAt return a non-nil error for a handle that was never Created.
func TestFileAtUnknownHandleIsHardError(t *testing.T) {
	t.Parallel()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	ctx := context.Background()
	ghost := container.Handle{Name: "ghost", ID: "/nonexistent/path"}

	if _, err := b.ReadFileAt(ctx, ghost, "foo.txt"); err == nil {
		t.Error("ReadFileAt unknown handle: err = nil, want non-nil error")
	}
	if err := b.WriteFileAt(ctx, ghost, "foo.txt", []byte("x")); err == nil {
		t.Error("WriteFileAt unknown handle: err = nil, want non-nil error")
	}
}
