//go:build integ

package docker

import (
	"context"
	"testing"

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
