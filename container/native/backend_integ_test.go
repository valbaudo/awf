//go:build integ

package native_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/backendtest"
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
	ch, resultCh, err := b.Exec(context.Background(), h, container.Cmd{Run: "echo HELLO"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	chunks := drain(ch)
	res := <-resultCh
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if got := string(res.Stdout); got != "HELLO\n" {
		t.Errorf("Stdout = %q, want %q", got, "HELLO\n")
	}
	if len(chunks) == 0 {
		t.Error("no IOChunks emitted")
	}
}

func TestNativeExecStderrCaptured(t *testing.T) {
	b, h := newBackendAndHandle(t)
	ch, resultCh, err := b.Exec(context.Background(), h, container.Cmd{Run: "echo OOPS >&2"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	chunks := drain(ch)
	<-resultCh
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
			ch, resultCh, err := b.Exec(context.Background(), h, container.Cmd{Run: c.run})
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			for range ch {
			}
			res := <-resultCh
			if res.ExitCode != c.want {
				t.Errorf("ExitCode = %d, want %d", res.ExitCode, c.want)
			}
		})
	}
}

func TestNativeExecEnvPassthrough(t *testing.T) {
	b, h := newBackendAndHandle(t)
	ch, resultCh, err := b.Exec(context.Background(), h, container.Cmd{
		Run: "echo $FOO",
		Env: map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range ch {
	}
	res := <-resultCh
	if got := strings.TrimSpace(string(res.Stdout)); got != "bar" {
		t.Errorf("Stdout = %q, want %q", got, "bar")
	}
	// Host env inherited — PATH must be present in child.
	ch, resultCh, err = b.Exec(context.Background(), h, container.Cmd{Run: "echo $PATH"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range ch {
	}
	res = <-resultCh
	if strings.TrimSpace(string(res.Stdout)) == "" {
		t.Error("PATH empty in child env; host env should be inherited")
	}
}

func TestNativeCaptureFilesRoundTrip(t *testing.T) {
	b, h := newBackendAndHandle(t)
	// Exec writes a file relative to workdir (cmd.Dir = workdir).
	ch, resultCh, err := b.Exec(context.Background(), h, container.Cmd{Run: "echo content > out.txt"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range ch {
	}
	<-resultCh
	files, err := b.CaptureFiles(context.Background(), h, []string{"out.txt"})
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	if got := string(files[0].Content); got != "content\n" {
		t.Errorf("Content = %q, want %q", got, "content\n")
	}
	// Also verify workdir resolution: the file was actually written under <workdir>/out.txt.
	if _, err := os.Stat(filepath.Join(h.ID, "out.txt")); err != nil {
		t.Errorf("expected workdir-relative file: %v", err)
	}
}

func TestNativeRunBasicContract(t *testing.T) {
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	backendtest.RunBasicContract(t, b)
}

// drain consumes all chunks from ch (expected to be closed before Exec returns).
func drain(ch <-chan container.IOChunk) []container.IOChunk {
	var out []container.IOChunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}
