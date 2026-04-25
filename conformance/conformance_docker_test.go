//go:build integ

package conformance

import (
	"fmt"
	"testing"
	"time"

	dockerclient "github.com/docker/docker/client"

	"github.com/valbaudo/awf/container/docker"
	"github.com/valbaudo/awf/state"
)

// TestConformanceDockerBackend is the Phase 4 conformance entry point.
// Invokes RunDockerSuite (Bucket 9/10/11) against a Docker Backend
// constructed via the production docker.New constructor.
//
// Gated by `//go:build integ` — only runs under `go test -tags integ`.
// The factory closure mints a fresh client + Backend per call, with
// t.Cleanup registered for orphan-sweep (any container/network/volume
// whose name starts with "awf-<runID>-") plus Backend.Close (releases
// the lazy composeCli's wrapped client) plus client.Close.
//
// Counterpart to TestConformanceFakeBackend (conformance_fake_test.go,
// slice 2.6), which runs RunSuite (Bucket 1-8) against the fake.
//
// In Task 2 (this commit), all bucket stubs t.Skip — the test runs end-to-end
// and reports SKIP for each sub-test. Tasks 3/4/5 unskip incrementally.
func TestConformanceDockerBackend(t *testing.T) {
	RunDockerSuite(t, func(t *testing.T, label string, opts ...docker.Option) DockerTestEnv {
		t.Helper()
		cli, err := dockerclient.NewClientWithOpts(
			dockerclient.FromEnv,
			dockerclient.WithAPIVersionNegotiation(),
		)
		if err != nil {
			t.Fatalf("docker client: %v", err)
		}
		// runID format matches container/docker/newTestBackend (slice 4.1) —
		// "test-<label>-<unix-nano>" — so orphan containers are greppable
		// with the same pattern across both test layers.
		runID := fmt.Sprintf("test-%s-%d", label, time.Now().UnixNano())
		blobs := state.NewInMemoryBlobs()
		b, err := docker.New(cli, runID, blobs, opts...)
		if err != nil {
			_ = cli.Close()
			t.Fatalf("docker.New: %v", err)
		}
		t.Cleanup(func() {
			cleanupDockerOrphans(t, cli, "awf-"+runID+"-")
			// Release composeCli's wrapped client BEFORE cli.Close —
			// matches the cli/backend.go production cleanup pattern AND
			// container/docker/newTestBackend's pattern (both added in
			// the post-slice-4.5 goleak fix, commit 55d42be). Without
			// this, compose-mode buckets leak HTTP transport goroutines
			// at test-process exit.
			_ = b.Close()
			_ = cli.Close()
		})
		return DockerTestEnv{Backend: b, Client: cli, Blobs: blobs}
	})
}
