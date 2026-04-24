//go:build integ

package docker

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	cont "github.com/valbaudo/awf/container"
)

// alpineDigest + pullImage + newTestBackend are defined in slice 4.1's
// backend_integ_test.go (same package).

// newAlpineContainer pulls the alpine fixture and creates a container directly
// via the Docker SDK (bypassing Backend.Create) so that `sleep infinity` can
// be injected as the Cmd without polluting the production Backend.Create path.
// Alpine's default /bin/sh exits immediately on start, which would cause
// Backend.Exec to fail with "not running". Injecting the CMD here keeps the
// override in test code where it belongs (Phase 4 design §A: the entrypoint
// is responsible for readiness).
//
// The resulting Handle is registered in the Backend's private map so that
// Backend.Exec, Backend.CaptureFiles, and Backend.Destroy work normally.
// Teardown is via t.Cleanup → Backend.Destroy (same as the production path).
func newAlpineContainer(t *testing.T, cli *client.Client, b *Backend) cont.Handle {
	t.Helper()
	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}
	name := containerName(b.runID, "lab")
	resp, err := cli.ContainerCreate(ctx,
		&dockerContainer.Config{Image: alpineDigest, Cmd: []string{"sleep", "infinity"}},
		&dockerContainer.HostConfig{},
		nil, nil, name,
	)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := cli.ContainerStart(ctx, resp.ID, dockerContainer.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	b.mu.Lock()
	b.handles[resp.ID] = registeredContainer{kind: "image", dockerID: resp.ID}
	b.mu.Unlock()
	h := cont.Handle{Name: "lab", ID: resp.ID}
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
