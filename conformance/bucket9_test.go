//go:build integ

package conformance

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/valbaudo/awf/container"
)

// testBucket9 runs the Phase 4 design §G "Bucket 9 — Docker (image-mode)"
// inventory. Migrated from container/docker/{backend,exec,capture}_integ_test.go
// (slices 4.1 + 4.2) in slice 4.6.
func testBucket9(t *testing.T, factory DockerBackendFactory) {
	t.Helper()
	t.Run("create_and_destroy", func(t *testing.T) { testBucket9CreateAndDestroy(t, factory) })
	t.Run("streamed_exec_demux", func(t *testing.T) { testBucket9ExecStdoutStderrDemux(t, factory) })
	t.Run("streamed_exec_ctx_cancel", func(t *testing.T) { testBucket9ExecCtxCancelMidRun(t, factory) })
	t.Run("streamed_exec_passes_env", func(t *testing.T) { testBucket9ExecPassesEnv(t, factory) })
	t.Run("capture_files_round_trip", func(t *testing.T) { testBucket9CaptureFilesRoundTrip(t, factory) })
	t.Run("capture_files_missing", func(t *testing.T) { testBucket9CaptureFilesMissing(t, factory) })
	t.Run("capture_files_ordering", func(t *testing.T) { testBucket9CaptureFilesOrdering(t, factory) })
	t.Run("capture_files_partial_missing", func(t *testing.T) { testBucket9CaptureFilesPartialMissing(t, factory) })
}

// testBucket9CreateAndDestroy migrates TestBucket9a_CreateAndDestroy from
// container/docker/backend_integ_test.go (slice 4.1).
func testBucket9CreateAndDestroy(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket9a-create")
	ctx := context.Background()
	if err := env.PullImage(ctx, AlpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}
	h, err := env.Backend.Create(ctx, container.ContainerSpec{
		Name:  "lab",
		Image: AlpineDigest,
		Cmd:   []string{"sleep", "infinity"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.Name != "lab" {
		t.Errorf("Handle.Name = %q, want \"lab\"", h.Name)
	}
	if h.ID == "" {
		t.Error("Handle.ID is empty")
	}
	if got := env.Backend.Capabilities().Snapshot; got != container.SnapshotFSCoW {
		t.Errorf("Capabilities().Snapshot = %v, want SnapshotFSCoW (docker)", got)
	}
	if err := env.Backend.Destroy(ctx, h); err != nil {
		t.Errorf("Destroy: %v", err)
	}
}

// testBucket9ExecStdoutStderrDemux migrates TestBucket9b_ExecStdoutStderrDemux
// (slice 4.2). Independent factory call to mirror the original's
// per-test isolation + orphan-debugging label.
func testBucket9ExecStdoutStderrDemux(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket9b-demux")
	h := env.NewAlpineHandle(t, "lab")
	ctx := context.Background()
	ch, resultCh, callErr := env.Backend.Exec(ctx, h, container.Cmd{
		Run: "echo OUT; echo ERR >&2; exit 7",
	})
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	var sawStdout, sawStderr bool
	for c := range ch {
		switch c.Stream {
		case "stdout":
			if bytes.Contains(c.Data, []byte("OUT")) {
				sawStdout = true
			}
		case "stderr":
			if bytes.Contains(c.Data, []byte("ERR")) {
				sawStderr = true
			}
		default:
			t.Errorf("IOChunk.Stream = %q, want stdout or stderr", c.Stream)
		}
	}
	result := <-resultCh
	if result.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", result.ExitCode)
	}
	if !bytes.Contains(result.Stdout, []byte("OUT")) {
		t.Errorf("Stdout = %q, want to contain OUT", result.Stdout)
	}
	if bytes.Contains(result.Stdout, []byte("ERR")) {
		t.Errorf("Stdout = %q, must NOT contain stderr content ERR", result.Stdout)
	}
	if !sawStdout {
		t.Error("no stdout chunk containing OUT")
	}
	if !sawStderr {
		t.Error("no stderr chunk containing ERR")
	}
}

// testBucket9ExecCtxCancelMidRun migrates TestBucket9b_ExecCtxCancelMidRun.
// Independent factory call; distinct orphan-debugging label.
func testBucket9ExecCtxCancelMidRun(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket9b-cancel")
	h := env.NewAlpineHandle(t, "lab")
	execCtx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	ch, resultCh, err := env.Backend.Exec(execCtx, h, container.Cmd{Run: "sleep 10"})
	if err != nil {
		t.Fatalf("Exec launch: err = %v, want nil (cancellation surfaces via ExecResult.Err)", err)
	}
	for range ch {
	}
	result := <-resultCh
	elapsed := time.Since(start)
	if result.Err == nil {
		t.Fatal("ExecResult.Err = nil, want context.Canceled")
	}
	if !errors.Is(result.Err, context.Canceled) {
		t.Errorf("ExecResult.Err = %v, want errors.Is(_, context.Canceled)", result.Err)
	}
	// Phase 4 design §G targets ctx-cancel mid-Exec to return within
	// 500ms. The test asserts <5s (verifies the property exists
	// without making CI flake on a transiently-slow daemon); the
	// 500ms SLA lives in the design spec, not the test ceiling.
	if elapsed > 5*time.Second {
		t.Errorf("Exec cancel-to-return latency = %v, want <5s (spec §G targets 500ms)", elapsed-200*time.Millisecond)
	}
}

// testBucket9ExecPassesEnv migrates TestBucket9b_ExecPassesEnv.
// Independent factory call; distinct orphan-debugging label.
func testBucket9ExecPassesEnv(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket9b-env")
	h := env.NewAlpineHandle(t, "lab")
	ctx := context.Background()
	ch, resultCh, callErr := env.Backend.Exec(ctx, h, container.Cmd{
		Run: `echo "key=$MY_KEY"`,
		Env: map[string]string{"MY_KEY": "value-42"},
	})
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	for range ch {
	}
	result := <-resultCh
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !bytes.Contains(result.Stdout, []byte("key=value-42")) {
		t.Errorf("Stdout = %q, want to contain key=value-42", result.Stdout)
	}
}

// testBucket9CaptureFiles* — migrated from
// container/docker/capture_integ_test.go (slice 4.2). Per-test factory
// calls match original's TestBucket9c_* per-test isolation.

func testBucket9CaptureFilesRoundTrip(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket9c-roundtrip")
	h := env.NewAlpineHandle(t, "lab")
	ctx := context.Background()
	// Preserved from original (capture_integ_test.go:13): content string
	// "hello captured world\n" and the files[0].Path echo-back assertion
	// (Path reflecting the requested path is contract behavior, not impl
	// detail — so it stays under conformance).
	want := []byte("hello captured world\n")
	ch, resultCh, err := env.Backend.Exec(ctx, h, container.Cmd{
		Run: `echo "hello captured world" > /tmp/awf-test.txt`,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range ch {
	}
	<-resultCh
	files, err := env.Backend.CaptureFiles(ctx, h, []string{"/tmp/awf-test.txt"})
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	if files[0].Path != "/tmp/awf-test.txt" {
		t.Errorf("Path = %q, want /tmp/awf-test.txt", files[0].Path)
	}
	if !bytes.Equal(files[0].Content, want) {
		t.Errorf("Content = %q, want %q", files[0].Content, want)
	}
}

func testBucket9CaptureFilesMissing(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket9c-missing")
	h := env.NewAlpineHandle(t, "lab")
	_, err := env.Backend.CaptureFiles(context.Background(), h, []string{"/nope"})
	if err == nil {
		t.Error("err = nil, want non-nil for missing path")
	}
}

func testBucket9CaptureFilesOrdering(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket9c-order")
	h := env.NewAlpineHandle(t, "lab")
	ctx := context.Background()
	// Preserved from original (capture_integ_test.go:50): path-based
	// ordering check (testing ordering by checking returned-path order is
	// direct; content-based is indirect) + the len(files) != 3 length
	// check + the original's fixture filenames (/tmp/a.txt, /tmp/b.txt,
	// /tmp/c.txt — NOT /tmp/a etc.).
	for _, name := range []string{"a", "b", "c"} {
		ch, resultCh, err := env.Backend.Exec(ctx, h, container.Cmd{Run: "echo " + name + " > /tmp/" + name + ".txt"})
		if err != nil {
			t.Fatalf("Exec write %s: %v", name, err)
		}
		for range ch {
		}
		<-resultCh
	}
	want := []string{"/tmp/c.txt", "/tmp/a.txt", "/tmp/b.txt"}
	files, err := env.Backend.CaptureFiles(ctx, h, want)
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("CaptureFiles: len = %d, want 3", len(files))
	}
	for i, w := range want {
		if files[i].Path != w {
			t.Errorf("files[%d].Path = %q, want %q", i, files[i].Path, w)
		}
	}
}

func testBucket9CaptureFilesPartialMissing(t *testing.T, factory DockerBackendFactory) {
	env := factory(t, "bucket9c-partial")
	h := env.NewAlpineHandle(t, "lab")
	ctx := context.Background()
	// Preserved from original (capture_integ_test.go:80): the
	// files != nil "no partial returns" assertion — documented contract
	// intent per the test's name + rationale: if even one path is
	// missing, the whole call errors with no partial returns.
	ch, resultCh, err := env.Backend.Exec(ctx, h, container.Cmd{Run: "echo present > /tmp/present.txt"})
	if err != nil {
		t.Fatalf("Exec write present: %v", err)
	}
	for range ch {
	}
	<-resultCh
	files, err := env.Backend.CaptureFiles(ctx, h, []string{"/tmp/present.txt", "/tmp/missing.txt"})
	if err == nil {
		t.Errorf("CaptureFiles partial-missing: err = nil, files = %+v, want non-nil err", files)
	}
	if files != nil {
		t.Errorf("CaptureFiles partial-missing: files = %+v, want nil (no partial returns)", files)
	}
}
