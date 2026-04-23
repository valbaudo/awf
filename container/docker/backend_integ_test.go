//go:build integ

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	cerrdefs "github.com/containerd/errdefs"

	cont "github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/backendtest"
)

// alpineDigest pins the fixture image. Update via:
//
//	docker pull alpine:3.20
//	docker inspect alpine:3.20 --format '{{index .RepoDigests 0}}'
const alpineDigest = "alpine@sha256:8a1f59ffb675680d47db6337b49d22281fcd6db88f2f5301f78ab3a08c1d3a12"

// newDockerClient is inlined here (rather than in a non-integ file) because
// slice 4.1's only consumer is integ-test code. A non-integ file with this
// helper would fail golangci-lint's `unused` check (default-tag build doesn't
// see integ-tagged callers). Slice 4.5 will introduce its own production
// client constructor at the CLI boundary.
//
// FromEnv reads DOCKER_HOST / DOCKER_TLS_VERIFY / etc.; WithAPIVersionNegotiation
// asks the SDK to ping the daemon at construction time and use whatever API
// version both sides agree on (avoids "client too new" / "client too old"
// hard-fails against older Docker installs on dev machines).
func newDockerClient() (*client.Client, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

func TestBucket9a_CreateAndDestroy(t *testing.T) {
	cli, b := newTestBackend(t, "bucket9a-create")

	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	spec := cont.ContainerSpec{
		Name:  "lab",
		Image: alpineDigest,
	}
	h, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.Name != "lab" {
		t.Errorf("Handle.Name = %q, want \"lab\"", h.Name)
	}
	if h.ID == "" {
		t.Errorf("Handle.ID empty")
	}

	// Verify the container exists from Docker's perspective.
	info, inspectErr := cli.ContainerInspect(ctx, h.ID)
	if inspectErr != nil {
		t.Fatalf("ContainerInspect: %v", inspectErr)
	}
	wantName := "/" + containerName(b.runID, "lab")
	if info.Name != wantName {
		t.Errorf("docker name = %q, want %q", info.Name, wantName)
	}

	if err := b.Destroy(ctx, h); err != nil {
		t.Errorf("Destroy: %v", err)
	}

	// Second destroy: error (matches the fake / os.File.Close convention).
	if err := b.Destroy(ctx, h); err == nil {
		t.Errorf("second Destroy returned nil; want error")
	}

	// The container is actually gone. cerrdefs.IsNotFound is the canonical
	// 404 detection in 2026 — both client.IsErrNotFound and the legacy
	// errdefs.IsNotFound are deprecated aliases that delegate here.
	if _, err := cli.ContainerInspect(ctx, h.ID); err == nil || !cerrdefs.IsNotFound(err) {
		t.Errorf("ContainerInspect after Destroy: err = %v, want cerrdefs.IsNotFound", err)
	}
}

// TestBucket9a_CreateAppliesResourceLimits verifies the Resources field on
// ContainerSpec actually translates to Docker host-config limits. Without
// this, a typo (NanoCPU vs NanoCPUs, or wrong byte unit) would silently
// no-op — a load-bearing concern when the workload may be a runaway agent.
//
// The exact field paths on the inspected container depend on the
// docker/docker SDK version pinned in go.mod. TDD: the first run errors with
// the right field-not-found / unexpected-value message; implementer adjusts.
func TestBucket9a_CreateAppliesResourceLimits(t *testing.T) {
	cli, b := newTestBackend(t, "bucket9a-limits")

	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	spec := cont.ContainerSpec{
		Name:  "limited",
		Image: alpineDigest,
		Resources: &cont.ContainerResources{
			CPU: "2",
			Mem: "256m",
		},
	}
	h, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(ctx, h) })

	info, err := cli.ContainerInspect(ctx, h.ID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if info.HostConfig == nil {
		t.Fatal("info.HostConfig is nil")
	}
	// NanoCPUs: 2 vCPU = 2 * 10^9.
	if got, want := info.HostConfig.NanoCPUs, int64(2_000_000_000); got != want {
		t.Errorf("HostConfig.NanoCPUs = %d, want %d", got, want)
	}
	// Memory: 256m = 256 * 1024 * 1024 = 268435456.
	if got, want := info.HostConfig.Memory, int64(268_435_456); got != want {
		t.Errorf("HostConfig.Memory = %d, want %d", got, want)
	}
}

func TestBucket9a_BackendtestContract(t *testing.T) {
	cli, b := newTestBackend(t, "bucket9a-contract")

	ctx := context.Background()
	if err := pullImage(ctx, cli, alpineDigest); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// backendtest.RunBasicContract calls Create with ContainerSpec{Name: ...}
	// without an Image. For Docker, that would fail; we wrap with an Image-
	// defaulting adapter so the contract test exercises the real Docker
	// backend.
	contract := &backendWithDefaultImage{Backend: b, defaultImage: alpineDigest}
	backendtest.RunBasicContract(t, contract)
}

// backendWithDefaultImage wraps Backend to supply a default Image when the
// caller's spec doesn't carry one. The basic-contract test (slice 2.2) was
// written for the fake (which ignores Image); the Docker backend needs one.
// This adapter bridges without changing backendtest's interface.
type backendWithDefaultImage struct {
	*Backend
	defaultImage string
}

func (a *backendWithDefaultImage) Create(ctx context.Context, spec cont.ContainerSpec) (cont.Handle, error) {
	if spec.Image == "" {
		spec.Image = a.defaultImage
	}
	return a.Backend.Create(ctx, spec)
}

// newTestBackend constructs a real Docker client + Backend with a unique
// runID per test. Registers t.Cleanup hooks for orphan sweep + client.Close.
// Returns (*client.Client, *Backend) — no cleanup func, since cleanup runs
// via t.Cleanup automatically (LIFO across nested t.Run; runs on t.Skip /
// t.Fatal from any goroutine).
func newTestBackend(t *testing.T, label string) (*client.Client, *Backend) {
	t.Helper()
	cli, err := newDockerClient()
	if err != nil {
		t.Fatalf("newDockerClient: %v", err)
	}
	runID := fmt.Sprintf("test-%s-%d", label, time.Now().UnixNano())
	b, err := New(cli, runID)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		cleanupOrphans(t, cli, containerPrefix(runID))
		_ = cli.Close()
	})
	return cli, b
}

// cleanupOrphans removes any container whose name starts with prefix.
// Idempotent; safety net for tests that crash between Create and Destroy.
func cleanupOrphans(t *testing.T, cli *client.Client, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := cli.ContainerList(ctx, dockerContainer.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", prefix)),
	})
	if err != nil {
		t.Logf("cleanupOrphans: ContainerList: %v", err)
		return
	}
	for _, c := range list {
		for _, n := range c.Names {
			if !strings.HasPrefix(strings.TrimPrefix(n, "/"), prefix) {
				continue
			}
			if err := cli.ContainerRemove(ctx, c.ID, dockerContainer.RemoveOptions{Force: true}); err != nil {
				t.Logf("cleanupOrphans: ContainerRemove(%s): %v", c.ID, err)
			}
			break
		}
	}
}

// pullImage ensures the digest is available locally. The Docker API returns
// HTTP 200 + a stream of JSON status messages; pull errors surface mid-
// stream as objects with errorDetail.message OR a top-level error field.
// io.Copy(io.Discard, reader) would silently swallow those — we MUST scan
// the JSON stream to surface errors with their actual message.
func pullImage(ctx context.Context, cli *client.Client, ref string) error {
	reader, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("ImagePull: %w", err)
	}
	defer reader.Close()

	type pullStatus struct {
		Error       string `json:"error,omitempty"`
		ErrorDetail struct {
			Message string `json:"message"`
		} `json:"errorDetail,omitempty"`
	}
	dec := json.NewDecoder(reader)
	for {
		var s pullStatus
		if err := dec.Decode(&s); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("ImagePull stream decode: %w", err)
		}
		if s.ErrorDetail.Message != "" {
			return fmt.Errorf("ImagePull: %s", s.ErrorDetail.Message)
		}
		if s.Error != "" {
			return fmt.Errorf("ImagePull: %s", s.Error)
		}
	}
}
