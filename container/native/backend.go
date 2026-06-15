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
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/state"
)

const (
	snapshotDefaultMaxBlobBytes    int64 = 256 << 20 // 256 MiB compressed
	snapshotDefaultMaxRestoreBytes int64 = 4 << 30   // 4 GiB decompressed (256 MiB × 16, under DEFLATE's ~1032× max ratio)
	snapshotMaxEntries             int   = 1_000_000 // entry-count cap (zip-bomb / inode-exhaustion guard)
)

// Backend implements container.Backend by spawning host processes via
// os/exec. Single concrete impl; not safe for concurrent runs of the
// same workflow (see slice 4.7 design spec Appendix E for the
// /tmp/awf/<step>.json clobber failure mode).
type Backend struct {
	workdirRoot string

	blobs                   state.Blobs
	snapshotMaxBlobBytes    int64
	snapshotMaxRestoreBytes int64

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
func New(workdirRoot string, opts ...Option) (*Backend, error) {
	if workdirRoot == "" {
		return nil, errors.New("container/native: New: workdirRoot is required")
	}
	if err := os.MkdirAll(container.AWFOutputDir, 0o755); err != nil {
		return nil, err
	}
	b := &Backend{
		workdirRoot:             workdirRoot,
		handles:                 map[string]nativeHandle{},
		snapshotMaxBlobBytes:    snapshotDefaultMaxBlobBytes,
		snapshotMaxRestoreBytes: snapshotDefaultMaxRestoreBytes,
	}
	for _, opt := range opts {
		if err := opt(b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Option configures a native Backend at construction.
type Option func(*Backend) error

// WithBlobs supplies the CAS store Snapshot/Restore use. Without it the backend
// advertises SnapshotNone and Snapshot returns a descriptive error.
func WithBlobs(b state.Blobs) Option {
	return func(n *Backend) error { n.blobs = b; return nil }
}

// WithSnapshotMaxBlobBytes overrides the compressed snapshot-blob cap (default
// snapshotDefaultMaxBlobBytes). n must be > 0.
func WithSnapshotMaxBlobBytes(n int64) Option {
	return func(b *Backend) error {
		if n <= 0 {
			return fmt.Errorf("container/native: WithSnapshotMaxBlobBytes: n must be > 0, got %d", n)
		}
		b.snapshotMaxBlobBytes = n
		return nil
	}
}

// WithSnapshotMaxRestoreBytes overrides the cumulative decompressed-bytes cap
// (default snapshotDefaultMaxRestoreBytes). n must be > 0.
func WithSnapshotMaxRestoreBytes(n int64) Option {
	return func(b *Backend) error {
		if n <= 0 {
			return fmt.Errorf("container/native: WithSnapshotMaxRestoreBytes: n must be > 0, got %d", n)
		}
		b.snapshotMaxRestoreBytes = n
		return nil
	}
}

// Capabilities reports SnapshotFSArchive iff blobs were supplied (WithBlobs),
// else SnapshotNone — native implements Snapshot/Restore only against an
// injected CAS store. Without it, workflows with snapshot:workspace containers
// must use --backend docker.
func (b *Backend) Capabilities() container.Caps {
	snap := container.SnapshotNone
	if b.blobs != nil {
		snap = container.SnapshotFSArchive
	}
	// RuntimeImage is false: native ignores spec.Image and runs on the host, so
	// a map's runtime image cannot be honored. The CLI guard rejects such a
	// workflow on native (P6a) — fail closed.
	return container.Caps{Snapshot: snap, RuntimeImage: false, RuntimeCompose: false}
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

// Restore returns ErrUnsupported — native does not implement
// filesystem snapshots (decision 4).
func (*Backend) Restore(_ context.Context, _ container.SnapshotRef, _ string) (container.Handle, error) {
	return container.Handle{}, container.ErrUnsupported
}

// Compile-time interface satisfaction.
var _ container.Backend = (*Backend)(nil)
