//go:build integ

package docker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// newComposeHandle pulls alpine, then Creates a compose-mode container from
// the named fixture and returns the Handle (auto-destroyed via t.Cleanup).
func newComposeHandle(t *testing.T, b *Backend, fixturePath, service string) cont.Handle {
	t.Helper()
	ctx := context.Background()
	if err := pullImage(ctx, b.cli, alpineDigest); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}

	spec := cont.ContainerSpec{
		Name:        "lab",
		Compose:     loadComposeFixture(t, fixturePath),
		ComposePath: fixturePath,
		Service:     service,
	}
	h, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create compose: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })
	return h
}

func TestBucket10a_ComposeBasicUpExecDown(t *testing.T) {
	_, b := newTestBackend(t, "bucket10a-basic")
	h := newComposeHandle(t, b, "cli/testdata/phase4/compose-basic.yml", "web")

	if h.ID != composeProjectName(b.runID) {
		t.Errorf("Handle.ID = %q, want %q", h.ID, composeProjectName(b.runID))
	}
	if h.Service != "web" {
		t.Errorf("Handle.Service = %q, want \"web\"", h.Service)
	}

	ctx := context.Background()
	result, ch, err := b.Exec(ctx, h, cont.Cmd{Run: "echo hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !bytes.Contains(result.Stdout, []byte("hello")) {
		t.Errorf("Stdout = %q, want to contain hello", result.Stdout)
	}
	for range ch {
	}
}

func TestBucket10b_ComposeCrossServiceExec(t *testing.T) {
	_, b := newTestBackend(t, "bucket10b-cross")
	h := newComposeHandle(t, b, "cli/testdata/phase4/compose-two-svc.yml", "web")

	ctx := context.Background()

	// Each service writes a unique marker to /tmp/awf-svc-marker (see fixture).
	// Reading it deterministically identifies which container an Exec landed in
	// — independent of Docker's undocumented hostname defaults.
	result, ch, err := b.Exec(ctx, h, cont.Cmd{Run: "cat /tmp/awf-svc-marker"})
	if err != nil {
		t.Fatalf("Exec web: %v", err)
	}
	for range ch {
	}
	if got := strings.TrimSpace(string(result.Stdout)); got != "web" {
		t.Errorf("default-service marker = %q, want \"web\"", got)
	}

	crossH := h
	crossH.Service = "db"
	result, ch, err = b.Exec(ctx, crossH, cont.Cmd{Run: "cat /tmp/awf-svc-marker"})
	if err != nil {
		t.Fatalf("Exec db (cross-service): %v", err)
	}
	for range ch {
	}
	if got := strings.TrimSpace(string(result.Stdout)); got != "db" {
		t.Errorf("cross-service marker = %q, want \"db\"", got)
	}
}

func TestBucket10c_ComposeUpWaitHonorsHealthcheck(t *testing.T) {
	_, b := newTestBackend(t, "bucket10c-wait")

	start := time.Now()
	h := newComposeHandle(t, b, "cli/testdata/phase4/compose-slow-ready.yml", "slow")
	elapsed := time.Since(start)

	if elapsed < 1500*time.Millisecond {
		t.Errorf("Create returned in %v; expected >=1.5s. up --wait may have skipped healthcheck gating.", elapsed)
	}
	if elapsed > 30*time.Second {
		t.Errorf("Create elapsed = %v; expected <30s (healthcheck should succeed within ~2-3s)", elapsed)
	}

	ctx := context.Background()
	result, ch, err := b.Exec(ctx, h, cont.Cmd{Run: "test -f /tmp/ready"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range ch {
	}
	if result.ExitCode != 0 {
		t.Errorf("test -f /tmp/ready: exit=%d, want 0 (service should be healthy)", result.ExitCode)
	}
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
