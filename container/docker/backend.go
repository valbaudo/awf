package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker/client"

	"github.com/valbaudo/awf/container"
)

// Backend is the Docker Engine SDK implementation of container.Backend.
// Slice 4.1 (Phase 4) ships the skeleton; later slices fill the four stubs.
//
// Concurrency: mu (added in Task 7) will protect handles once Create and
// Destroy have real implementations. Slice 4.1 omits it to avoid an
// "unused field" vet error while the only mutators are panicking stubs.
type Backend struct {
	cli   *client.Client
	runID string

	handles map[string]string // handle.ID → docker container id; guarded by mu (Task 7)
}

// New constructs a Docker Backend bound to a specific run. Both arguments
// are required: cli for daemon talk, runID for the container-name prefix
// per Phase 4 design decision 9 (parallel runs on one host MUST NOT
// collide; the per-run prefix guarantees this).
//
// Slice 4.1 callers construct cli via
// client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation());
// slice 4.5 will introduce a shared helper at the CLI boundary.
func New(cli *client.Client, runID string) (*Backend, error) {
	if cli == nil {
		return nil, errors.New("container/docker: New: cli is required")
	}
	if runID == "" {
		return nil, errors.New("container/docker: New: runID is required (used for container naming)")
	}
	return &Backend{
		cli:     cli,
		runID:   runID,
		handles: map[string]string{},
	}, nil
}

// Capabilities advertises SnapshotFSCoW. The actual Snapshot implementation
// lands in slice 4.4; until then, Snapshot returns *ErrNotImplementedInSlice41.
// backendtest.testSnapshotRouting / testRestoreRouting both skip when the
// backend advertises non-SnapshotNone, so the stub does NOT violate the
// basic contract.
func (*Backend) Capabilities() container.Caps {
	return container.Caps{Snapshot: container.SnapshotFSCoW}
}

// Create is implemented in Task 7 (the integ-test task) — it requires the
// real daemon. Slice 4.1's Capabilities + stubs are testable here, but
// Create + Destroy are tested in backend_integ_test.go.
func (b *Backend) Create(ctx context.Context, spec container.ContainerSpec) (container.Handle, error) {
	// Implementation in Task 7.
	panic("Create: not yet implemented; landing in Task 7")
}

// Destroy is implemented in Task 7.
func (b *Backend) Destroy(ctx context.Context, h container.Handle) error {
	// Implementation in Task 7.
	panic("Destroy: not yet implemented; landing in Task 7")
}

// Exec is stubbed — slice 4.2.
func (b *Backend) Exec(ctx context.Context, h container.Handle, cmd container.Cmd) (container.ExecResult, <-chan container.IOChunk, error) {
	if err := ctx.Err(); err != nil {
		return container.ExecResult{}, nil, err
	}
	return container.ExecResult{}, nil, &ErrNotImplementedInSlice41{Method: "Exec"}
}

// CaptureFiles is stubbed — slice 4.2.
func (b *Backend) CaptureFiles(ctx context.Context, h container.Handle, paths []string) ([]container.CapturedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, &ErrNotImplementedInSlice41{Method: "CaptureFiles"}
}

// Snapshot is stubbed — slice 4.4.
func (b *Backend) Snapshot(ctx context.Context, h container.Handle) (container.SnapshotRef, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", &ErrNotImplementedInSlice41{Method: "Snapshot"}
}

// Restore is stubbed — slice 4.4.
func (b *Backend) Restore(ctx context.Context, ref container.SnapshotRef) (container.Handle, error) {
	if err := ctx.Err(); err != nil {
		return container.Handle{}, err
	}
	return container.Handle{}, &ErrNotImplementedInSlice41{Method: "Restore"}
}

// ErrNotImplementedInSlice41 is the sentinel returned by methods that exist
// on the Backend (for interface satisfaction) but have no implementation in
// slice 4.1. Slice 4.2 (Exec, CaptureFiles) and 4.4 (Snapshot, Restore)
// replace the stubs; routing via errors.As(err, &ErrNotImplementedInSlice41{})
// lets those slices detect the replacement is complete.
type ErrNotImplementedInSlice41 struct {
	Method string
}

func (e *ErrNotImplementedInSlice41) Error() string {
	return fmt.Sprintf("container/docker: %s is not implemented in slice 4.1 (see docs/superpowers/specs/2026-04-14-awf-phase4-design.md)", e.Method)
}

// Compile-time interface satisfaction.
var _ container.Backend = (*Backend)(nil)
