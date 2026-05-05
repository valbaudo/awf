//go:build integ && live

package conformance

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	dockerclient "github.com/docker/docker/client"

	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/docker"
	"github.com/valbaudo/awf/state"
)

// TestConformanceAgentClaudeDocker drives RunAgentSuite against the
// Docker backend + the compose lab. Bucket 14a runs against the lab
// container's installed claude; Bucket 14c runs gate-repair-cve.yaml
// end-to-end.
//
// Auth env: claude inside the lab container reads env vars forwarded
// by the slice-5.3 adapter (claude.WithEnv).
//
// Skips when docker unreachable OR no auth env var. Host claude is NOT
// required — the lab container installs its own.
func TestConformanceAgentClaudeDocker(t *testing.T) {
	skipIfNoAuth(t)

	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("docker client: %v", err)
	}
	if _, err := cli.Ping(context.Background()); err != nil {
		t.Skipf("docker ping: %v", err)
	}

	repoRoot := conformanceRepoRoot()
	composePath := filepath.Join(repoRoot, "cli", "testdata", "phase5", "oracle", "compose.yml")
	composeBytes, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read compose.yml: %v", err)
	}

	dockerFactory := func(t *testing.T) AgentTestEnv {
		t.Helper()
		// dockerRunID is the docker.Backend's runID — used for
		// container naming via "awf-<dockerRunID>-..." (see
		// container/docker/backend.go:179). INTENTIONALLY distinct
		// from the AWF run ID set by bucket14c.go's clock.Fake (used
		// for the log path under <stateDir>/runs/<awfRunID>/).
		dockerRunID := "conf-agent-docker-" + t.Name()
		blobs := state.NewInMemoryBlobs()
		db, err := docker.New(cli, dockerRunID, blobs)
		if err != nil {
			t.Fatalf("docker.New: %v", err)
		}
		ad, err := claude.New(
			claude.WithEnv(envFromHost(claude.DefaultEnvAllowlist)),
			claude.WithBackend(db),
		)
		if err != nil {
			t.Fatalf("claude.New: %v", err)
		}
		// Compose-project cleanup on test panic — reuses the existing
		// conformance helper at docker_suite_test.go:238. Matches the
		// container-name prefix the docker.Backend uses.
		cleanupDockerOrphans(t, cli, "awf-"+dockerRunID)
		return AgentTestEnv{
			Backend: db,
			Adapter: ad,
			Spec: container.ContainerSpec{
				Name:        "lab",
				Compose:     composeBytes,
				ComposePath: composePath,
				Service:     "lab",
			},
		}
	}

	RunAgentSuite(t, dockerFactory)
}
