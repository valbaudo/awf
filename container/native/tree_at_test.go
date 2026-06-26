package native_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

// TestTreeAtRoundTrip verifies that WriteTreeAt followed by ReadTreeAt
// returns the same relative paths and content. Uses the claude-session
// subtree path from the task-2 brief to confirm the expected use-case.
func TestTreeAtRoundTrip(t *testing.T) {
	t.Parallel()
	b, h := newTestBackendAndHandle(t)
	ctx := context.Background()

	const dir = ".awf/claude-session/RUN/projects"

	// Build a tar with two files relative to dir.
	orig, err := container.BuildTreeTar(map[string][]byte{
		"session.jsonl":     []byte(`{"role":"user","content":"hello"}`),
		"subdir/other.json": []byte(`{"v":1}`),
	})
	if err != nil {
		t.Fatalf("BuildTreeTar: %v", err)
	}

	if err := b.WriteTreeAt(ctx, h, dir, orig); err != nil {
		t.Fatalf("WriteTreeAt: %v", err)
	}

	// Verify the files landed on disk at the expected location.
	workdir := filepath.Join(newTestWorkdirRoot(t, b, h), "ws")
	if data, err := os.ReadFile(filepath.Join(workdir, ".awf", "claude-session", "RUN", "projects", "session.jsonl")); err != nil {
		t.Fatalf("session.jsonl not on disk: %v", err)
	} else if string(data) != `{"role":"user","content":"hello"}` {
		t.Errorf("session.jsonl content = %q, want original", data)
	}

	// Read it back and verify the tar round-trips.
	got, err := b.ReadTreeAt(ctx, h, dir)
	if err != nil {
		t.Fatalf("ReadTreeAt: %v", err)
	}

	files, err := container.ExtractTreeTar(got, container.TreeTarMaxBytes, container.TreeTarMaxEntries)
	if err != nil {
		t.Fatalf("ExtractTreeTar on ReadTreeAt output: %v", err)
	}
	if string(files["session.jsonl"]) != `{"role":"user","content":"hello"}` {
		t.Errorf("round-tripped session.jsonl = %q, want original", files["session.jsonl"])
	}
	if string(files["subdir/other.json"]) != `{"v":1}` {
		t.Errorf("round-tripped subdir/other.json = %q, want original", files["subdir/other.json"])
	}
}

// TestTreeAtWriteEscapeRejected verifies that WriteTreeAt refuses a tar
// containing a symlink that points outside the workdir root followed by a
// regular entry that would traverse through it. Mirrors
// TestRestoreSymlinkTraversalConfined in snapshot_test.go.
func TestTreeAtWriteEscapeRejected(t *testing.T) {
	t.Parallel()
	b, h := newTestBackendAndHandle(t)
	ctx := context.Background()

	victimDir := t.TempDir()
	tarGz := buildMaliciousTreeTar(t, victimDir)

	err := b.WriteTreeAt(ctx, h, ".awf/claude-session/RUN/projects", tarGz)
	if err == nil {
		t.Fatal("WriteTreeAt with escape tar: err = nil, want confinement error")
	}

	// The sentinel file must NOT have been written outside the workdir root.
	if _, statErr := os.Stat(filepath.Join(victimDir, "x")); statErr == nil {
		t.Fatal("symlink-based write escaped os.Root: victimDir/x was created")
	}
}

// TestTreeAtReadMissingDirErrors verifies that ReadTreeAt returns a non-nil
// error when the requested directory does not exist in the workdir.
func TestTreeAtReadMissingDirErrors(t *testing.T) {
	t.Parallel()
	b, h := newTestBackendAndHandle(t)
	ctx := context.Background()

	_, err := b.ReadTreeAt(ctx, h, ".awf/does-not-exist/projects")
	if err == nil {
		t.Error("ReadTreeAt on missing dir: err = nil, want non-nil error")
	}
}

// TestTreeAtUnknownHandleIsHardError verifies that both methods return a
// non-nil error for an unknown handle, and — critically for WriteTreeAt —
// the handle is checked BEFORE tar decoding work begins. Passing invalid
// tar bytes ensures the only possible cause of a non-nil error is the
// handle lookup itself.
func TestTreeAtUnknownHandleIsHardError(t *testing.T) {
	t.Parallel()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	ctx := context.Background()
	ghost := container.Handle{Name: "ghost", ID: "/nonexistent/path"}
	badTar := []byte("not a valid gzip-tar")

	if _, err := b.ReadTreeAt(ctx, ghost, "some/dir"); err == nil {
		t.Error("ReadTreeAt unknown handle: err = nil, want non-nil error")
	}

	// Pass invalid tar bytes: if handle check came AFTER gzip decode, the error
	// would mention gzip/tar, not the handle. Correct ordering gives
	// "unknown handle" from the handle lookup before any tar work.
	writeErr := b.WriteTreeAt(ctx, ghost, "some/dir", badTar)
	if writeErr == nil {
		t.Fatal("WriteTreeAt unknown handle: err = nil, want non-nil error")
	}
	if !strings.Contains(writeErr.Error(), "unknown handle") {
		t.Errorf("WriteTreeAt unknown handle: expected 'unknown handle' in error, got: %v", writeErr)
	}
}

// newTestWorkdirRoot extracts the workdir root from a backend+handle pair for
// on-disk assertions. It exploits the fact that the native backend stores
// the workdir as the Handle.ID (which is <workdirRoot>/<name>).
// This helper is purely for test verification — it reads Handle.ID.
func newTestWorkdirRoot(t *testing.T, _ *native.Backend, h container.Handle) string {
	t.Helper()
	// Handle.ID is the absolute path to the container workdir.
	// filepath.Dir gives the workdirRoot.
	return filepath.Dir(h.ID)
}

// buildMaliciousTreeTar builds a gzip-tar that contains:
//  1. A symlink entry "evil" pointing to victimDir (an absolute path outside
//     any workdir root), and
//  2. A regular file "evil/x" with content "pwned".
//
// A naive extractor would use the symlink to write victimDir/x; an
// os.Root-confined extractor must refuse the follow-through.
func buildMaliciousTreeTar(t *testing.T, victimDir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	// Entry 1: symlink "evil" → victimDir (absolute path outside the root).
	if err := tw.WriteHeader(&tar.Header{
		Name:     "evil",
		Typeflag: tar.TypeSymlink,
		Linkname: victimDir,
		Mode:     0o777,
	}); err != nil {
		t.Fatal(err)
	}
	// Entry 2: regular file through the symlink.
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
	return buf.Bytes()
}
