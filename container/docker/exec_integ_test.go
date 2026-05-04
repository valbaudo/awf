//go:build integ

package docker

import (
	"context"
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
	b.handles[resp.ID] = registeredContainer{kind: kindImage, dockerID: resp.ID}
	b.mu.Unlock()
	h := cont.Handle{Name: "lab", ID: resp.ID}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })
	return h
}

// TestDocker_Exec_ChunksArriveDuringExec verifies the slice-5.3 streaming
// contract on the docker backend: chunks arrive progressively over the wire
// (not after the command exits). The script prints 5 lines spaced 100ms
// apart; we record the wall-clock arrival of each stdout chunk and assert
// the first→last span is ≥200ms — impossible if chunks were buffered until
// process exit and then bursted.
func TestDocker_Exec_ChunksArriveDuringExec(t *testing.T) {
	cli, b := newTestBackend(t, "5.3-streaming")
	h := newAlpineContainer(t, cli, b)

	cmd := cont.Cmd{Run: "for i in 1 2 3 4 5; do echo line-$i; sleep 0.1; done"}
	start := time.Now()
	chunks, result, err := b.Exec(context.Background(), h, cmd)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var arrivals []time.Duration
	for c := range chunks {
		if c.Stream == "stdout" {
			arrivals = append(arrivals, time.Since(start))
		}
	}
	if len(arrivals) < 2 {
		t.Fatalf("got %d stdout chunks; want >=2", len(arrivals))
	}
	if span := arrivals[len(arrivals)-1] - arrivals[0]; span < 200*time.Millisecond {
		t.Errorf("first->last span = %v; want >=200ms (chunks must arrive progressively)", span)
	}
	r := <-result
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d; want 0", r.ExitCode)
	}
	if r.Err != nil {
		t.Errorf("ExecResult.Err = %v; want nil", r.Err)
	}
}
