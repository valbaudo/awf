//go:build integ

package docker

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/docker/docker/client"

	cont "github.com/valbaudo/awf/container"
)

// alpineDigest + pullImage + newTestBackend are defined in slice 4.1's
// backend_integ_test.go (same package).

// newAlpineContainer pulls the alpine fixture and Creates a container; the
// returned Handle is auto-destroyed via t.Cleanup. With slice 4.2's Backend
// using `sh -c` (Design Q5), alpine works without a bash bootstrap.
func newAlpineContainer(t *testing.T, cli *client.Client, b *Backend) cont.Handle {
	t.Helper()
	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}
	h, err := b.Create(ctx, cont.ContainerSpec{Name: "lab", Image: alpineDigest})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })
	return h
}

func TestBucket9b_ExecStdoutStderrDemux(t *testing.T) {
	cli, b := newTestBackend(t, "bucket9b-demux")
	h := newAlpineContainer(t, cli, b)
	ctx := context.Background()

	// Fixture: echo to stdout AND stderr. The order is stdout first, then
	// stderr — stdcopy demux preserves stream identity but the order on the
	// wire may interleave so we don't assert a specific chunk-count, only
	// that BOTH streams produced at least one chunk with the expected content.
	result, ch, callErr := b.Exec(ctx, h, cont.Cmd{
		Run: "echo OUT; echo ERR >&2; exit 7",
	})
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	if result.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", result.ExitCode)
	}
	if !bytes.Contains(result.Stdout, []byte("OUT")) {
		t.Errorf("Stdout = %q, want to contain OUT", result.Stdout)
	}
	if bytes.Contains(result.Stdout, []byte("ERR")) {
		t.Errorf("Stdout = %q, must NOT contain stderr content ERR", result.Stdout)
	}
	// Verify stream tagging on the chunks.
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
	if !sawStdout {
		t.Error("no stdout chunk containing OUT")
	}
	if !sawStderr {
		t.Error("no stderr chunk containing ERR")
	}
}

func TestBucket9b_ExecCtxCancelMidRun(t *testing.T) {
	cli, b := newTestBackend(t, "bucket9b-cancel")
	h := newAlpineContainer(t, cli, b)
	ctx := context.Background()

	// A long-running command we'll cancel mid-flight.
	execCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, _, err := b.Exec(execCtx, h, cont.Cmd{Run: "sleep 10"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Exec: err = nil, want context.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Exec: err = %v, want errors.Is(_, context.Canceled)", err)
	}
	// Phase 4 design §G targets ctx-cancel mid-Exec to return within 500ms.
	// The test asserts <5s (verifies the property exists without making CI
	// flake on a transiently-slow daemon); the 500ms SLA lives in the design
	// spec, not the test ceiling.
	if elapsed > 5*time.Second {
		t.Errorf("Exec cancel-to-return latency = %v, want <5s (spec §G targets 500ms)", elapsed-200*time.Millisecond)
	}
}

func TestBucket9b_ExecPassesEnv(t *testing.T) {
	cli, b := newTestBackend(t, "bucket9b-env")
	h := newAlpineContainer(t, cli, b)
	ctx := context.Background()

	result, _, callErr := b.Exec(ctx, h, cont.Cmd{
		Run: `echo "key=$MY_KEY"`,
		Env: map[string]string{"MY_KEY": "value-42"},
	})
	if callErr != nil {
		t.Fatalf("Exec: %v", callErr)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !bytes.Contains(result.Stdout, []byte("key=value-42")) {
		t.Errorf("Stdout = %q, want to contain key=value-42", result.Stdout)
	}
}
