// Package native implements container.Backend by running commands
// directly on the host via os/exec. It ignores the IR's image: field
// and refuses compose: containers. Native runs are explicitly out-of-spec
// for digest-pinned reproducibility and are NOT resumable (see
// cli/backend.go:readBackendKindFromLog for the resume-side guard).
//
// Design: docs/superpowers/specs/2026-04-20-awf-phase4-slice-4-7-design.md
//
// /tmp/awf bootstrap: New() creates host /tmp/awf because the engine
// dispatcher writes AWF_OUTPUT to /tmp/awf/<step>.json (engine/awf_output.go:24-26)
// and the author's shell step `... > $AWF_OUTPUT` requires the directory
// to exist. Docker doesn't need this (each container's /tmp is fresh).
//
// CaptureFiles `..` traversal: resolves via filepath.Join's cleaning
// rules. A path like ../etc/passwd reads from the host literally.
// Consistent with the no-isolation design (Cmd.Run is equally
// unconstrained).
package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/valbaudo/awf/container"
)

// awfOutputDir is the host directory the dispatcher writes AWF_OUTPUT
// tempfiles into. Created once at New() per Phase 4 slice 4.7 decision 5.
const awfOutputDir = "/tmp/awf"

// Backend implements container.Backend by spawning host processes via
// os/exec. Single concrete impl; not safe for concurrent runs of the
// same workflow (see slice 4.7 design spec Appendix E for the
// /tmp/awf/<step>.json clobber failure mode).
type Backend struct {
	workdirRoot string

	mu      sync.Mutex
	handles map[string]nativeHandle
}

// nativeHandle is the per-Create internal state. Stored in
// Backend.handles keyed by Handle.ID.
type nativeHandle struct {
	workdir string
}

// New constructs a Backend with workdirRoot as the parent for per-
// container workdirs. Also bootstraps host /tmp/awf (idempotent).
// Validates only nil/empty workdirRoot — filesystem errors surface
// lazily from Create's MkdirAll, matching docker's pattern.
//
// Edge case: pre-existing /tmp/awf with restrictive perms (e.g.,
// root-owned 0o700) is NOT detected here — first Exec fails with
// shell "permission denied" instead.
func New(workdirRoot string) (*Backend, error) {
	if workdirRoot == "" {
		return nil, errors.New("container/native: New: workdirRoot is required")
	}
	if err := os.MkdirAll(awfOutputDir, 0o755); err != nil {
		return nil, err
	}
	return &Backend{
		workdirRoot: workdirRoot,
		handles:     map[string]nativeHandle{},
	}, nil
}

// Capabilities reports SnapshotNone — native does not implement
// Snapshot/Restore. Workflows with snapshot:workspace containers
// must use --backend docker.
func (*Backend) Capabilities() container.Caps {
	return container.Caps{Snapshot: container.SnapshotNone}
}

// Create rejects compose-mode (no service-routing on host), ignores
// spec.Image (native is not digest-pinned per decision 1), and creates
// <workdirRoot>/<spec.Name>/ as the per-container workdir. The Handle's
// ID carries the workdir path so Exec/CaptureFiles/Destroy can resolve
// it via the handles map.
func (b *Backend) Create(ctx context.Context, spec container.ContainerSpec) (container.Handle, error) {
	if err := ctx.Err(); err != nil {
		return container.Handle{}, err
	}
	if spec.Compose != nil {
		return container.Handle{}, errors.New("container/native: Create: compose-mode not supported (use --backend docker for compose workflows)")
	}
	workdir := filepath.Join(b.workdirRoot, spec.Name)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return container.Handle{}, err
	}
	b.mu.Lock()
	b.handles[workdir] = nativeHandle{workdir: workdir}
	b.mu.Unlock()
	return container.Handle{Name: spec.Name, ID: workdir}, nil
}

// Destroy removes the handle's workdir and unregisters the handle.
// Returns an error on double-destroy (matches docker / os.File.Close
// convention). os.RemoveAll is idempotent on ENOENT, so a workdir
// that's already gone (e.g., a step deleted it) succeeds.
func (b *Backend) Destroy(ctx context.Context, h container.Handle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	if !ok {
		b.mu.Unlock()
		return errors.New("container/native: Destroy: unknown handle (already destroyed or never Created)")
	}
	delete(b.handles, h.ID)
	b.mu.Unlock()
	return os.RemoveAll(r.workdir)
}

// Snapshot returns ErrUnsupported — native does not implement
// filesystem snapshots (decision 4). Workflows with snapshot:workspace
// containers must use --backend docker.
func (*Backend) Snapshot(_ context.Context, _ container.Handle) (container.SnapshotRef, error) {
	return "", container.ErrUnsupported
}

// Restore returns ErrUnsupported — native does not implement
// filesystem snapshots (decision 4).
func (*Backend) Restore(_ context.Context, _ container.SnapshotRef, _ string) (container.Handle, error) {
	return container.Handle{}, container.ErrUnsupported
}

// Compile-time interface satisfaction.
var _ container.Backend = (*Backend)(nil)
