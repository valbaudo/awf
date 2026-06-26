// Package native implements container.Backend by running commands
// directly on the host via os/exec. It ignores the IR's image: field
// and refuses compose: containers. Native runs are resumable —
// snapshot: workspace workdirs are restored from a full gzip-tar archive
// on resume; the host base environment is not pinned, so native remains
// explicitly out-of-spec for digest-pinned reproducibility.
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
	"os/exec"
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
	maxEntries              int

	// sandbox is the resolved launcher for WS-5. nil means sandbox was not
	// requested (WithSandbox(false) or not supplied); non-nil means sandbox
	// was requested and detectSandbox resolved a launcher (may be the no-op
	// fallback if no platform sandbox was available).
	sandbox sandboxLauncher

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
		maxEntries:              snapshotMaxEntries,
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

// WithSandbox enables (enabled=true) or disables (enabled=false) OS-level
// process sandboxing for native backend Exec calls. When enabled, detectSandbox
// resolves the best available platform launcher (bwrap on Linux, sandbox-exec
// on macOS). If no platform sandbox is available the backend falls back to a
// no-op launcher and SandboxWarnLabel returns a non-empty loud warning string
// that the CLI MUST surface to stderr (mirror of cli/run.go:354 pattern).
//
// When disabled (or not supplied), sandbox detection is skipped entirely and
// SandboxWarnLabel returns "".
func WithSandbox(enabled bool) Option {
	return func(b *Backend) error {
		if !enabled {
			return nil
		}
		l, _ := detectSandbox(exec.LookPath)
		b.sandbox = l
		return nil
	}
}

// SandboxWarnLabel returns a non-empty warning string when the backend was
// constructed with WithSandbox(true) but no platform sandbox was available
// (i.e. the no-op fallback is in use). Returns "" when sandbox is disabled
// or when a real platform launcher was selected.
//
// The CLI MUST print this to stderr immediately after constructing the native
// backend, in the same style as the no-image warning at cli/run.go:354:
//
//	awf run: --backend native: <SandboxWarnLabel()>
func (b *Backend) SandboxWarnLabel() string {
	if b.sandbox == nil {
		return "" // sandbox not enabled
	}
	if _, ok := b.sandbox.(noOpLauncher); ok {
		return noSandboxWarnLabel
	}
	return "" // real launcher — no warning
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
	//
	// StagingRoot is ".awf" (workdir-relative). native.CopyTo joins relative
	// paths to the container's workdir (r.workdir), so a relative root lands
	// under the per-container workdir instead of the literal host path
	// "/work/.awf" (which does not exist on the host).
	return container.Caps{Snapshot: snap, RuntimeImage: false, RuntimeCompose: false, StagingRoot: ".awf"}
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

// Restore re-materializes a container workdir from a SnapshotRef; it lives in
// snapshot.go (os.Root-confined extraction).

// ResolveWorkdirPath implements container.WorkdirResolver. It returns
// filepath.Join(workdir, rel) where workdir is the absolute host path of the
// handle's working directory. If the handle is unknown (defensive), rel is
// returned unchanged — the method never panics.
func (b *Backend) ResolveWorkdirPath(h container.Handle, rel string) string {
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return rel
	}
	return filepath.Join(r.workdir, rel)
}

// Compile-time interface satisfaction.
var _ container.Backend = (*Backend)(nil)
var _ container.WorkdirResolver = (*Backend)(nil)
