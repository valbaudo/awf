//go:build integ

package native_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

func newBackendAndHandle(t *testing.T) (*native.Backend, container.Handle) {
	t.Helper()
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(context.Background(), h) })
	return b, h
}

func TestNativeExecStdoutCaptured(t *testing.T) {
	b, h := newBackendAndHandle(t)
	res, ch, err := b.Exec(context.Background(), h, container.Cmd{Run: "echo HELLO"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if got := string(res.Stdout); got != "HELLO\n" {
		t.Errorf("Stdout = %q, want %q", got, "HELLO\n")
	}
	// Channel must be closed and pre-filled (Phase 2 contract).
	chunks := drain(ch)
	if len(chunks) == 0 {
		t.Error("no IOChunks emitted")
	}
}

func TestNativeExecStderrCaptured(t *testing.T) {
	b, h := newBackendAndHandle(t)
	_, ch, err := b.Exec(context.Background(), h, container.Cmd{Run: "echo OOPS >&2"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	chunks := drain(ch)
	var stderrSeen bool
	for _, c := range chunks {
		if c.Stream == "stderr" && strings.Contains(string(c.Data), "OOPS") {
			stderrSeen = true
		}
	}
	if !stderrSeen {
		t.Errorf("expected stderr IOChunk with 'OOPS', got chunks=%+v", chunks)
	}
}

func TestNativeExecExitCode(t *testing.T) {
	cases := []struct {
		name string
		run  string
		want int
	}{
		{"zero", "true", 0},
		{"one", "false", 1},
		{"sigkill-via-exit", "exit 137", 137},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, h := newBackendAndHandle(t)
			res, _, err := b.Exec(context.Background(), h, container.Cmd{Run: c.run})
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if res.ExitCode != c.want {
				t.Errorf("ExitCode = %d, want %d", res.ExitCode, c.want)
			}
		})
	}
}

func TestNativeExecEnvPassthrough(t *testing.T) {
	b, h := newBackendAndHandle(t)
	res, _, err := b.Exec(context.Background(), h, container.Cmd{
		Run: "echo $FOO",
		Env: map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "bar" {
		t.Errorf("Stdout = %q, want %q", got, "bar")
	}
	// Host env inherited — PATH must be present in child.
	res, _, err = b.Exec(context.Background(), h, container.Cmd{Run: "echo $PATH"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(string(res.Stdout)) == "" {
		t.Error("PATH empty in child env; host env should be inherited")
	}
}

// Note: TestNativeCaptureFilesRoundTrip and TestNativeRunBasicContract are
// intentionally NOT added here — they require CaptureFiles which is
// implemented in Task 6. Adding them here would leave Task 5's commit with
// red tests in the suite. Task 6 adds them when their dependencies are ready
// (TDD: each task leaves the suite green).

// drain consumes all chunks from ch (expected to be closed before Exec returns).
func drain(ch <-chan container.IOChunk) []container.IOChunk {
	var out []container.IOChunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}

// Suppress unused-import warnings for imports that Task 6 will use.
var _ = os.Stat
var _ = filepath.Join
