//go:build integ

package docker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	cont "github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/backendtest"
)

func loadComposeFixture(t *testing.T, path string) []byte {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", path)) // container/docker → repo root
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read fixture %s: %v", abs, err)
	}
	return data
}

// backendWithDefaultCompose wraps Backend to supply default Compose bytes +
// ComposePath + Service when the caller's spec doesn't carry them. The basic-
// contract test (slice 2.2) was written for the fake (which ignores everything
// except Name); the Docker compose-mode Backend needs the compose-mode fields.
type backendWithDefaultCompose struct {
	*Backend
	defaultCompose     []byte
	defaultComposePath string
	defaultService     string
}

func (a *backendWithDefaultCompose) Create(ctx context.Context, spec cont.ContainerSpec) (cont.Handle, error) {
	if spec.Compose == nil && spec.Image == "" {
		spec.Compose = a.defaultCompose
		spec.ComposePath = a.defaultComposePath
		spec.Service = a.defaultService
	}
	return a.Backend.Create(ctx, spec)
}

func TestBucket10_BackendtestContractCompose(t *testing.T) {
	_, b := newTestBackend(t, "bucket10-contract")

	ctx := context.Background()
	if err := pullImage(ctx, b.cli, alpineDigest); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}

	contract := &backendWithDefaultCompose{
		Backend:            b,
		defaultCompose:     loadComposeFixture(t, "cli/testdata/phase4/compose-basic.yml"),
		defaultComposePath: "cli/testdata/phase4/compose-basic.yml",
		defaultService:     "web",
	}
	backendtest.RunBasicContract(t, contract)
}

func TestCreateComposeCleansUpProjectOnUpFailure(t *testing.T) {
	cli, b := newTestBackend(t, "runtime-compose-up-fail")
	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}

	spec := cont.ContainerSpec{
		Name: "runtime-bad",
		Compose: []byte(`
services:
  web:
    image: ` + alpineDigest + `
    command: ["sh", "-c", "sleep 86400"]
  bad:
    image: ` + alpineDigest + `
    command: ["sh", "-c", "exit 42"]
`),
		ComposePath: "runtime-bad.yml",
		Service:     "web",
	}
	if _, err := b.Create(ctx, spec); err == nil {
		t.Fatal("Create returned nil error for a compose project with an exiting service")
	}

	assertNoComposeProjectResources(t, cli, composeProjectName(b.runID, spec.Name))
}

func assertNoComposeProjectResources(t *testing.T, cli *client.Client, project string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	containers, err := cli.ContainerList(ctx, dockerContainer.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project)),
	})
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	if len(containers) != 0 {
		t.Fatalf("found %d leftover compose container(s) for project %q", len(containers), project)
	}

	networks, err := cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", project)),
	})
	if err != nil {
		t.Fatalf("NetworkList: %v", err)
	}
	for _, n := range networks {
		if n.Name == project || n.Name == project+"_default" {
			t.Fatalf("found leftover compose network %q for project %q", n.Name, project)
		}
	}

	volumes, err := cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", project)),
	})
	if err != nil {
		t.Fatalf("VolumeList: %v", err)
	}
	for _, v := range volumes.Volumes {
		if v != nil && len(v.Name) >= len(project) && v.Name[:len(project)] == project {
			t.Fatalf("found leftover compose volume %q for project %q", v.Name, project)
		}
	}
}
