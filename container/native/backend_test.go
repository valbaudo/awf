package native_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
	"github.com/valbaudo/awf/state"
)

func TestNativeCapsNoBlobsIsSnapshotNone(t *testing.T) {
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.Capabilities().Snapshot; got != container.SnapshotNone {
		t.Errorf("Snapshot caps without blobs = %q, want %q", got, container.SnapshotNone)
	}
}

func TestNativeCapsWithBlobsIsArchive(t *testing.T) {
	b, err := native.New(t.TempDir(), native.WithBlobs(state.NewInMemoryBlobs()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.Capabilities().Snapshot; got != container.SnapshotFSArchive {
		t.Errorf("Snapshot caps with blobs = %q, want %q", got, container.SnapshotFSArchive)
	}
}

func TestNativeWithSnapshotMaxBlobBytesRejectsNonPositive(t *testing.T) {
	if _, err := native.New(t.TempDir(), native.WithSnapshotMaxBlobBytes(0)); err == nil {
		t.Error("WithSnapshotMaxBlobBytes(0): err = nil, want non-nil")
	}
}

// TestNative_AWFOutput_UnderWorkdir is the U3/F25 round-trip: AWF_OUTPUT is
// now workdir-relative (Caps.OutputRoot == ".awf/output"), not the old
// process-global host /tmp/awf (see the deleted TestNewBootstrapsTmpAwf and
// the removed New() bootstrap). Create must pre-create <workdir>/.awf/output
// so the author's `> $AWF_OUTPUT` redirect succeeds, and the write must
// survive to a subsequent CaptureFiles call.
func TestNative_AWFOutput_UnderWorkdir(t *testing.T) {
	root := t.TempDir()
	b, err := native.New(root)
	if err != nil {
		t.Fatal(err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	// Emulate the dispatcher: AWF_OUTPUT is the workdir-relative rooted path.
	rel := ".awf/output/step.json"
	chunks, resCh, err := b.Exec(context.Background(), h, container.Cmd{
		Run: `echo '{"ok":true}' > "$AWF_OUTPUT"`,
		Env: map[string]string{"AWF_OUTPUT": rel},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range chunks {
	}
	if r := <-resCh; r.ExitCode != 0 {
		t.Fatalf("exit=%d", r.ExitCode)
	}
	got, err := b.CaptureFiles(context.Background(), h, []string{rel})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if string(got[0].Content) != "{\"ok\":true}\n" {
		t.Fatalf("content=%q", got[0].Content)
	}
}

func TestNewRejectsEmptyWorkdirRoot(t *testing.T) {
	t.Parallel()
	_, err := native.New("")
	if err == nil {
		t.Fatal("err = nil, want non-nil for empty workdirRoot")
	}
}

func TestCloseReleasesRootAndIsIdempotent(t *testing.T) {
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := b.Create(context.Background(), container.ContainerSpec{Name: "after-close"}); err == nil {
		t.Fatal("Create after Close: error = nil, want closed-root error")
	}
}

func TestCanonicalRelativeWorkdirRoot(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	b, err := native.New(filepath.Join(".awf", "work", "run"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(cwd, ".awf", "work", "run", "lab"))
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != want {
		t.Fatalf("Handle.ID = %q, want canonical absolute path %q", h.ID, want)
	}
	if !filepath.IsAbs(h.ID) {
		t.Fatalf("Handle.ID = %q, want absolute path", h.ID)
	}
	resolved := b.ResolveWorkdirPath(h, ".awf/output/result.json")
	if !filepath.IsAbs(resolved) {
		t.Fatalf("ResolveWorkdirPath = %q, want absolute path", resolved)
	}
}

func TestCanonicalSymlinkedWorkdirRoot(t *testing.T) {
	cwd := t.TempDir()
	realRoot := filepath.Join(cwd, "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(cwd, "state")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	b, err := native.New(filepath.Join("state", "work"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(realRoot, "work", "lab"))
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != want {
		t.Fatalf("Handle.ID = %q, want symlink-resolved path %q", h.ID, want)
	}
	if got := b.ResolveWorkdirPath(h, "result.json"); got != filepath.Join(want, "result.json") {
		t.Fatalf("ResolveWorkdirPath = %q, want %q", got, filepath.Join(want, "result.json"))
	}
}

func TestCreateRejectsUnsafeContainerName(t *testing.T) {
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{"", ".", "..", "../escape", "a/b", filepath.Join(string(filepath.Separator), "absolute")} {
		t.Run(strings.ReplaceAll(name, string(filepath.Separator), "_"), func(t *testing.T) {
			if _, err := b.Create(context.Background(), container.ContainerSpec{Name: name}); err == nil {
				t.Fatalf("Create(Name: %q) error = nil, want unsafe-name error", name)
			}
		})
	}
}

func TestCapabilitiesReturnsSnapshotNone(t *testing.T) {
	t.Parallel()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.Capabilities().Snapshot; got != container.SnapshotNone {
		t.Errorf("Capabilities().Snapshot = %v, want SnapshotNone", got)
	}
	if b.Capabilities().RuntimeCompose {
		t.Error("Capabilities().RuntimeCompose = true, want false (native cannot promote compose projects)")
	}
}

func TestCreateRejectsComposeSpec(t *testing.T) {
	t.Parallel()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Create(context.Background(), container.ContainerSpec{
		Name:    "lab",
		Compose: []byte("services: {}"),
	})
	if err == nil {
		t.Fatal("err = nil, want non-nil (compose rejected)")
	}
	if !strings.Contains(err.Error(), "compose-mode not supported") {
		t.Errorf("err = %q, want substring 'compose-mode not supported'", err)
	}
	if !strings.Contains(err.Error(), "container/native:") {
		t.Errorf("err = %q, want prefix 'container/native:'", err)
	}
}

func TestCreateIgnoresImageAndMakesWorkdir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	b, err := native.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{
		Name:  "lab",
		Image: "oci://example/image@sha256:abc",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.Name != "lab" {
		t.Errorf("Handle.Name = %q, want \"lab\"", h.Name)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "lab"))
	if err != nil {
		t.Fatalf("resolve expected workdir: %v", err)
	}
	if h.ID != want {
		t.Errorf("Handle.ID = %q, want %q (workdir path)", h.ID, want)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatalf("workdir not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("workdir is not a directory")
	}
}

func TestCreateBareSpecAccepted(t *testing.T) {
	t.Parallel()
	// ContainerSpec{Name: "lab"} (no Image, no Compose) — passed by
	// backendtest.RunBasicContract. Native must accept and create a workdir.
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.Name != "lab" || h.ID == "" {
		t.Errorf("Handle = %+v", h)
	}
}

func TestDestroyRemovesWorkdir(t *testing.T) {
	t.Parallel()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(h.ID); err != nil {
		t.Fatalf("workdir missing pre-Destroy: %v", err)
	}
	if err := b.Destroy(context.Background(), h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(h.ID); !os.IsNotExist(err) {
		t.Errorf("workdir still present after Destroy: err=%v", err)
	}
}

func TestDoubleDestroyErrors(t *testing.T) {
	t.Parallel()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Destroy(context.Background(), h); err != nil {
		t.Fatalf("first Destroy: %v", err)
	}
	if err := b.Destroy(context.Background(), h); err == nil {
		t.Error("second Destroy returned nil, want error (handle gone)")
	}
}

// TestResolveWorkdirPath verifies that native.Backend implements
// container.WorkdirResolver and that:
//   - a known handle returns filepath.Join(workdir, rel)
//   - an unknown handle returns rel unchanged (defensive, never panics)
func TestResolveWorkdirPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	b, err := native.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	wr, ok := any(b).(container.WorkdirResolver)
	if !ok {
		t.Fatal("native.Backend does not implement container.WorkdirResolver")
	}

	const rel = ".awf/claude-session/RUN"
	// h.ID is the workdir path for the native backend.
	want := filepath.Join(h.ID, rel)
	if got := wr.ResolveWorkdirPath(h, rel); got != want {
		t.Errorf("ResolveWorkdirPath(known) = %q, want %q", got, want)
	}

	// Unknown handle: must return rel unchanged (defensive — never panic).
	ghost := container.Handle{ID: "ghost-never-created"}
	if got := wr.ResolveWorkdirPath(ghost, rel); got != rel {
		t.Errorf("ResolveWorkdirPath(unknown) = %q, want %q (rel unchanged)", got, rel)
	}
}
