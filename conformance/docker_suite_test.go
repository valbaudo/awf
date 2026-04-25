//go:build integ

// Package conformance — Docker suite (slice 4.6).
//
// This file defines the Docker counterpart to suite.go's RunSuite: a
// separate entry point (RunDockerSuite) that runs the Docker-specific
// Buckets 9/10/11 against a real container.Backend produced by docker.New.
//
// Double-gated: `_test.go` suffix excludes the file from `go build` /
// `go install` (test-only); `//go:build integ` further excludes it from
// `make test` (which uses no tags). Compiles in only under
// `go test -tags integ`. The base conformance package (Bucket 1-8
// against fake) stays Docker-free. See slice 4.6 plan design Q2 for the
// rationale (idiomatic Go convention for test-only code; matches
// container/docker/*_integ_test.go).

package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/docker"
	"github.com/valbaudo/awf/state"
)

// AlpineDigest pins the fixture image the Docker buckets are built around.
// Centralized here (parallel to container/docker's package-private
// alpineDigest) so the conformance buckets don't need to reach into
// container/docker's internals. The two constants must stay in sync;
// update both when refreshing alpine:
//
//	docker manifest inspect alpine:latest | jq -r '.manifests[0].digest'
const AlpineDigest = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// DockerTestEnv is the per-test fixture a DockerBackendFactory mints.
// Fields are exposed (not method-wrapped) so bucket bodies can call
// Backend.Exec / Backend.Snapshot directly without an indirection layer
// — the env struct is plumbing, not abstraction.
//
//   - Backend: fresh per factory call; t.Cleanup registered by the factory
//     closure (orphan sweep + client.Close).
//   - Client: the raw Docker client the Backend wraps; exposed so buckets
//     can pre-pull images via PullImage.
//   - Blobs: the in-memory state.Blobs Backend writes Snapshot tars into;
//     buckets that assert on Blobs state read from this.
type DockerTestEnv struct {
	Backend container.Backend
	Client  *client.Client
	Blobs   state.Blobs
}

// PullImage ensures ref is available in the local image cache. Wraps
// pullDockerImage (which decodes errorDetail from the streamed JSON —
// io.Copy(io.Discard) would silently swallow registry errors). Method on
// the env for ergonomics; pullDockerImage is the plain-function form.
func (e DockerTestEnv) PullImage(ctx context.Context, ref string) error {
	return pullDockerImage(ctx, e.Client, ref)
}

// NewAlpineHandle creates a fresh alpine container with `sleep infinity`
// as the CMD (so Backend.Exec has something to attach to). Registers
// t.Cleanup → Backend.Destroy.
//
// Cmd matches container/docker/exec_integ_test.go's newAlpineContainer
// verbatim — sleep infinity works on Alpine's BusyBox sleep ≥ 1.30, which
// every alpine 3.x image ships. The slice-4.5 fixture convention uses
// `sh -c "sleep 86400"` for compose YAML (YAML escaping considerations),
// but for a Go Cmd []string the direct `sleep infinity` form is the
// established helper pattern.
//
// This version uses Backend.Create (not raw ContainerCreate via the
// Docker SDK like newAlpineContainer does) so the production code path
// is exercised and the Handle is canonically registered in the Backend's
// internal map. This is possible because slice 4.4 added
// ContainerSpec.Cmd; the original helper predates that field.
func (e DockerTestEnv) NewAlpineHandle(t *testing.T, name string) container.Handle {
	t.Helper()
	ctx := context.Background()
	if err := e.PullImage(ctx, AlpineDigest); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}
	h, err := e.Backend.Create(ctx, container.ContainerSpec{
		Name:  name,
		Image: AlpineDigest,
		Cmd:   []string{"sleep", "infinity"},
	})
	if err != nil {
		t.Fatalf("Create alpine %q: %v", name, err)
	}
	t.Cleanup(func() { _ = e.Backend.Destroy(ctx, h) })
	return h
}

// NewComposeHandle pulls alpine, then constructs a compose-mode container
// from composeBytes + repoRelativePath. Registers t.Cleanup → Destroy.
// Bucket 10 bodies use loadComposeFixture to obtain composeBytes from
// cli/testdata/phase4/*.yml; repoRelativePath is the SAME string passed
// to loadComposeFixture (the engine threads it through to compose's
// relative-path resolution).
//
// container.ContainerSpec is flat (Compose []byte + ComposePath string +
// Service string — slice 4.3 work). There is no ComposeSpec struct.
func (e DockerTestEnv) NewComposeHandle(t *testing.T, name string, composeBytes []byte, repoRelativePath, service string) container.Handle {
	t.Helper()
	ctx := context.Background()
	if err := e.PullImage(ctx, AlpineDigest); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}
	h, err := e.Backend.Create(ctx, container.ContainerSpec{
		Name:        name,
		Compose:     composeBytes,
		ComposePath: repoRelativePath,
		Service:     service,
	})
	if err != nil {
		t.Fatalf("Create compose %q: %v", name, err)
	}
	t.Cleanup(func() { _ = e.Backend.Destroy(ctx, h) })
	return h
}

// loadComposeFixture reads a fixture file by its repo-relative path. The
// caller passes a path rooted at the repo (e.g.
// "cli/testdata/phase4/compose-basic.yml"); this function resolves to
// absolute via filepath.Join("..", path) — `go test` runs with the
// package directory as CWD, so ".." walks one level to repo root.
//
// Matches container/docker/compose_integ_test.go:loadComposeFixture's
// pattern verbatim (only difference: that one uses "..", "..", path since
// it's two levels deep). Single source of truth at cli/testdata/phase4 —
// no fixture duplication.
//
// Fragile to a future move of conformance/ to a non-top-level location;
// if that ever happens, update both this helper AND
// container/docker/compose_integ_test.go's equivalent. The fragility
// class is acceptable per slice 4.6 design Q3.
func loadComposeFixture(t *testing.T, repoRelativePath string) []byte {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", repoRelativePath))
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read fixture %s: %v", abs, err)
	}
	return data
}

// DockerBackendFactory mints a fresh Docker-backed test environment per
// call. label is appended to the run-id prefix (matches
// container/docker/newTestBackend's pattern: "test-<label>-<unix-nano>")
// so orphan containers from a failed test are greppable on the host.
// opts forward to docker.New — Bucket 11c uses
// docker.WithSnapshotMaxBlobBytes(1024).
type DockerBackendFactory func(t *testing.T, label string, opts ...docker.Option) DockerTestEnv

// RunDockerSuite is the Docker-specific conformance entry point. Sub-tests
// run independently — each calls factory(t, ...) to get its own fresh
// Backend, so a hang in one bucket doesn't corrupt another's setup.
//
// Phase 4 spec §G inventory:
//
//   - bucket9_image_mode: Backend Create/Destroy + streamed Exec
//     stdout/stderr demux + CaptureFiles round-trip.
//   - bucket10_compose_mode: compose up --wait + service routing +
//     cross-service exec + healthcheck-gated readiness.
//   - bucket11_snapshot_restore: workspace mutation + restore + deleted
//     file + oversize diff hits ErrSnapshotTooLarge.
//
// Slice 4.6 wires all 9 sub-tests; see bucket9_test.go / bucket10_test.go
// / bucket11_test.go for the bodies.
func RunDockerSuite(t *testing.T, factory DockerBackendFactory) {
	t.Helper()
	t.Run("bucket9_image_mode", func(t *testing.T) { testBucket9(t, factory) })
	t.Run("bucket10_compose_mode", func(t *testing.T) { testBucket10(t, factory) })
	t.Run("bucket11_snapshot_restore", func(t *testing.T) { testBucket11(t, factory) })
}

// --- Docker plumbing helpers ---
//
// These are duplicated bodies of container/docker/*_integ_test.go
// package-private helpers. Duplication beats re-exporting from
// container/docker (CLAUDE.md "seams as designed — no more"; the
// container/docker public API stays narrowly Backend-shaped). The
// duplicates are short (~80 lines combined) and the originals are stable
// (slice 4.1-4.4 hasn't touched the helper bodies since they landed).

func pullDockerImage(ctx context.Context, cli *client.Client, ref string) error {
	reader, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("ImagePull: %w", err)
	}
	defer func() { _ = reader.Close() }()

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

// cleanupDockerOrphans removes containers/networks/volumes whose name
// starts with prefix. Adapted from container/docker/cleanupOrphans
// (extended in slice 4.3 to cover networks + volumes). Safety net for
// tests that crash between Create and Destroy.
func cleanupDockerOrphans(t *testing.T, cli *client.Client, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Containers.
	cList, err := cli.ContainerList(ctx, dockerContainer.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", prefix)),
	})
	if err != nil {
		t.Logf("cleanupDockerOrphans: ContainerList: %v", err)
	} else {
		for _, c := range cList {
			for _, n := range c.Names {
				if !strings.HasPrefix(strings.TrimPrefix(n, "/"), prefix) {
					continue
				}
				if err := cli.ContainerRemove(ctx, c.ID, dockerContainer.RemoveOptions{Force: true}); err != nil {
					t.Logf("cleanupDockerOrphans: ContainerRemove(%s): %v", c.ID, err)
				}
				break
			}
		}
	}

	// Networks (compose creates `<project>_default` and named ones).
	nList, err := cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", prefix)),
	})
	if err != nil {
		t.Logf("cleanupDockerOrphans: NetworkList: %v", err)
	} else {
		for _, n := range nList {
			if !strings.HasPrefix(n.Name, prefix) {
				continue
			}
			if err := cli.NetworkRemove(ctx, n.ID); err != nil {
				t.Logf("cleanupDockerOrphans: NetworkRemove(%s): %v", n.Name, err)
			}
		}
	}

	// Volumes.
	vList, err := cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", prefix)),
	})
	if err != nil {
		t.Logf("cleanupDockerOrphans: VolumeList: %v", err)
	} else {
		for _, v := range vList.Volumes {
			if v == nil || !strings.HasPrefix(v.Name, prefix) {
				continue
			}
			if err := cli.VolumeRemove(ctx, v.Name, true); err != nil {
				t.Logf("cleanupDockerOrphans: VolumeRemove(%s): %v", v.Name, err)
			}
		}
	}
}

// --- Stubs invoked by RunDockerSuite — bodies land in bucket{9,10,11}_test.go ---
//
// The parameter is named `_` (blank identifier) rather than `factory` to
// communicate that these stubs intentionally do not use it. Tasks 3/4/5
// replace these with calls to factory(...) when their bucket lands.
