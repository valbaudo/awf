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

	// mu guards handles. Add together with the first Create/Destroy
	// write in Task 3 (currently elided to satisfy golangci-lint
	// `unused` while the map has no writers).
	// mu sync.Mutex
	handles map[string]nativeHandle
}

// nativeHandle is the per-Create internal state. Stored in
// Backend.handles keyed by Handle.ID. Fields added in Tasks 3-5.
type nativeHandle struct{}

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

// Stub implementations — Create/Destroy/Exec/CaptureFiles replaced in
// Tasks 3-6. Snapshot/Restore keep ErrUnsupported (decision 4).

func (b *Backend) Create(_ context.Context, _ container.ContainerSpec) (container.Handle, error) {
	return container.Handle{}, errors.New("container/native: Create: not implemented (stub)")
}

func (b *Backend) Destroy(_ context.Context, _ container.Handle) error {
	return errors.New("container/native: Destroy: not implemented (stub)")
}

func (b *Backend) Exec(_ context.Context, _ container.Handle, _ container.Cmd) (container.ExecResult, <-chan container.IOChunk, error) {
	return container.ExecResult{}, nil, errors.New("container/native: Exec: not implemented (stub)")
}

func (b *Backend) CaptureFiles(_ context.Context, _ container.Handle, _ []string) ([]container.CapturedFile, error) {
	return nil, errors.New("container/native: CaptureFiles: not implemented (stub)")
}

func (b *Backend) Snapshot(_ context.Context, _ container.Handle) (container.SnapshotRef, error) {
	return "", container.ErrUnsupported
}

func (b *Backend) Restore(_ context.Context, _ container.SnapshotRef, _ string) (container.Handle, error) {
	return container.Handle{}, container.ErrUnsupported
}

// Compile-time interface satisfaction.
var _ container.Backend = (*Backend)(nil)
