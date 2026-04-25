//go:build integ

package docker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
