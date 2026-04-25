package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v2/pkg/api"
	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	units "github.com/docker/go-units"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/state"
)

// containerKind is the discriminator for registeredContainer. Defined as a
// typed string so all kind comparisons and assignments are type-checked at
// compile time; a typo like "Image" or "compose-mode" becomes a build error
// rather than a runtime default-branch fallback.
type containerKind string

const (
	kindImage   containerKind = "image"
	kindCompose containerKind = "compose"
)

// registeredContainer is the value type of Backend.handles. The discriminator
// kind says how to resolve the Docker resource for Exec / Destroy / capture:
//
//   - kind == kindImage — dockerID is the docker container ID; Exec passes it
//     directly to ContainerExecCreate. project / defaultSvc / composeAPI are
//     empty.
//   - kind == kindCompose — project is the compose project name (matches
//     Handle.ID); defaultSvc is the IR-declared default service; composeAPI is
//     the api.Compose instance the Create call used (cached for Down). Exec
//     resolves the target container via ContainerList(filter: project+service)
//     where service comes from Handle.Service if non-empty else defaultSvc.
//
// Slice 4.3 Task 3 introduces this type with kindImage only; Task 4 adds
// kindCompose populated entries. Image-mode behavior is preserved bytewise
// — only the lookup layer changed.
type registeredContainer struct {
	kind       containerKind
	dockerID   string      // image-mode
	project    string      // compose-mode
	defaultSvc string      // compose-mode
	composeAPI api.Compose // compose-mode; nil for image-mode
}

// Backend is the Docker Engine SDK implementation of container.Backend.
// Slice 4.1 ships the skeleton; 4.2 added real Exec + CaptureFiles; 4.3 adds
// compose-mode Create + Exec + Destroy; 4.4 adds Snapshot + Restore.
//
// blobs is the content-addressed artifact store used by Snapshot/Restore
// (slice 4.4). Required at construction time.
//
// snapshotMaxBlobBytes caps the gzip-compressed diff-tar blob size used by
// Snapshot (slice 4.4). Default snapshotDefaultMaxBlobBytes (256 MiB);
// override via WithSnapshotMaxBlobBytes. The Snapshot impl lands in Task 4;
// this commit ships the configurable cap.
type Backend struct {
	cli   *client.Client
	runID string
	blobs state.Blobs

	snapshotMaxBlobBytes int64

	mu      sync.Mutex
	handles map[string]registeredContainer

	// composeOnce + composeCli + composeErr together implement thread-safe
	// lazy initialization of the docker/cli command.Cli (needed by
	// compose.NewComposeService). Phase 3's engine/parallel.go (errgroup)
	// and engine/map.go (`go func()`) can call Backend.Create concurrently —
	// a bare `if cli == nil { init }` would race. sync.Once gives us
	// race-free init with the error cached so a once-failed init doesn't
	// retry.
	composeOnce sync.Once   // lazy compose.Cli init (compose-mode)
	composeCli  command.Cli // compose-mode
	composeErr  error       // compose-mode
}

// Option is a functional option for New. Idiomatic fallible-option pattern
// (gRPC, OTel, Cobra precedent).
type Option func(*Backend) error

// WithSnapshotMaxBlobBytes overrides the default Snapshot blob-size cap
// (256 MiB). The cap is on the GZIP-COMPRESSED tar bytes (the blob that
// goes through state.Blobs.Put), NOT on the underlying workspace mutation.
// Text-heavy workspaces compress 3-10×, so 256 MiB of compressed blob
// corresponds to roughly 1 GiB of typical text content.
//
// Rejects n <= 0 (operator footgun: WithSnapshotMaxBlobBytes(0) would
// make every Snapshot fail; explicit rejection at New time surfaces the
// typo loudly rather than at the first Snapshot call).
func WithSnapshotMaxBlobBytes(n int64) Option {
	return func(b *Backend) error {
		if n <= 0 {
			return fmt.Errorf("container/docker: WithSnapshotMaxBlobBytes: n must be > 0, got %d", n)
		}
		b.snapshotMaxBlobBytes = n
		return nil
	}
}

const snapshotDefaultMaxBlobBytes int64 = 256 << 20 // 256 MiB (compressed)

// New constructs a Docker Backend bound to a specific run.
//
//   - cli:   the Docker Engine client.
//   - runID: per-run container-name prefix (Phase 4 design decision 9).
//   - blobs: content-addressed artifact store for Snapshot/Restore.
//   - opts:  optional behavior overrides (today: WithSnapshotMaxBlobBytes).
//
// All three required args are nil-checked. Slice 4.5's CLI run constructor
// will pass the same Blobs the dispatcher uses at commit time.
func New(cli *client.Client, runID string, blobs state.Blobs, opts ...Option) (*Backend, error) {
	if cli == nil {
		return nil, errors.New("container/docker: New: cli is required")
	}
	if runID == "" {
		return nil, errors.New("container/docker: New: runID is required (used for container naming)")
	}
	if blobs == nil {
		return nil, errors.New("container/docker: New: blobs is required (Snapshot/Restore Put/Get against it)")
	}
	b := &Backend{
		cli:                  cli,
		runID:                runID,
		blobs:                blobs,
		handles:              map[string]registeredContainer{},
		snapshotMaxBlobBytes: snapshotDefaultMaxBlobBytes,
	}
	for _, opt := range opts {
		if err := opt(b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Capabilities advertises SnapshotFSCoW per Phase 4 design decision 4. The
// real Snapshot + Restore implementations live in snapshot.go (slice 4.4).
func (*Backend) Capabilities() container.Caps {
	return container.Caps{Snapshot: container.SnapshotFSCoW}
}

// Create materialises a container from the digest-pinned image in spec.Image,
// starts it, waits for readiness (image healthcheck or immediate if none), and
// returns a Handle. spec.Image is required for image-mode; compose mode lands
// in slice 4.3.
//
// Precondition: spec.Image must already be present in the local Docker
// image cache. Create does NOT pull — it calls cli.ContainerCreate which
// returns a "no such image" error if absent. Callers (slice 4.5's
// cli/run.go onward) are responsible for pre-pulling via client.ImagePull
// before invoking Create. The integ tests demonstrate the pattern via
// the pullImage helper.
func (b *Backend) Create(ctx context.Context, spec container.ContainerSpec) (container.Handle, error) {
	if err := ctx.Err(); err != nil {
		return container.Handle{}, err
	}

	// Compose-mode dispatch.
	if spec.Compose != nil {
		return b.createCompose(ctx, spec)
	}

	// Image-mode (existing slice 4.1 path).
	if spec.Image == "" {
		return container.Handle{}, fmt.Errorf("container/docker: Create: spec.Image is required for image-mode (or set spec.Compose for compose-mode)")
	}

	name := containerName(b.runID, spec.Name)
	cfg := &dockerContainer.Config{
		Image: spec.Image,
		Cmd:   spec.Cmd, // nil/empty → image default applies (slice 4.4 Q9)
	}
	hostCfg := &dockerContainer.HostConfig{}
	if spec.Resources != nil {
		if spec.Resources.CPU != "" {
			cpu, err := strconv.Atoi(spec.Resources.CPU)
			if err != nil {
				return container.Handle{}, fmt.Errorf("container/docker: Create: invalid Resources.CPU %q (want integer vCPU count): %w", spec.Resources.CPU, err)
			}
			if cpu > 0 {
				hostCfg.NanoCPUs = int64(cpu) * nanoCPUsPerCPU
			}
		}
		if spec.Resources.Mem != "" {
			bytes, err := units.RAMInBytes(spec.Resources.Mem)
			if err != nil {
				return container.Handle{}, fmt.Errorf("container/docker: Create: invalid Resources.Mem %q: %w", spec.Resources.Mem, err)
			}
			hostCfg.Memory = bytes
		}
	}

	// v28.5.2 ContainerCreate signature: positional (ctx, config, hostConfig,
	// networkingConfig, platform, containerName). Returns container.CreateResponse
	// with the new container ID.
	resp, err := b.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return container.Handle{}, fmt.Errorf("container/docker: Create: ContainerCreate: %w", err)
	}

	if err := b.cli.ContainerStart(ctx, resp.ID, dockerContainer.StartOptions{}); err != nil {
		// Cleanup the created-but-not-started container.
		_ = b.cli.ContainerRemove(ctx, resp.ID, dockerContainer.RemoveOptions{Force: true})
		return container.Handle{}, fmt.Errorf("container/docker: Create: ContainerStart: %w", err)
	}

	// If the image declares a healthcheck, wait until healthy. Otherwise the
	// entrypoint exit-from-init IS the readiness (spec §3 / Phase 4 design §A).
	if err := b.waitReady(ctx, resp.ID); err != nil {
		_ = b.cli.ContainerRemove(ctx, resp.ID, dockerContainer.RemoveOptions{Force: true})
		return container.Handle{}, err
	}

	b.mu.Lock()
	b.handles[resp.ID] = registeredContainer{kind: kindImage, dockerID: resp.ID}
	b.mu.Unlock()

	return container.Handle{Name: spec.Name, ID: resp.ID}, nil
}

// Destroy force-removes the container associated with h. Returns an error if h
// was never Created or was already Destroyed (matches the fake / os.File.Close
// double-close convention).
func (b *Backend) Destroy(ctx context.Context, h container.Handle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	b.mu.Lock()
	r, ok := b.handles[h.ID]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("container/docker: Destroy: unknown handle %q (already destroyed or never Created)", h.ID)
	}
	delete(b.handles, h.ID)
	b.mu.Unlock()

	switch r.kind {
	case kindImage:
		if err := b.cli.ContainerRemove(ctx, r.dockerID, dockerContainer.RemoveOptions{Force: true}); err != nil {
			// Re-record the handle so the caller can retry.
			b.mu.Lock()
			b.handles[h.ID] = r
			b.mu.Unlock()
			return fmt.Errorf("container/docker: Destroy: ContainerRemove: %w", err)
		}
		return nil
	case kindCompose:
		// NOTE: on error, the handle is NOT re-recorded (compose state may be
		// partially-down; cleanupOrphans is the safety net). See destroyCompose
		// doc-comment for the asymmetry vs image-mode above.
		return b.destroyCompose(ctx, r)
	default:
		return fmt.Errorf("container/docker: Destroy: unknown handle kind %q (engine bug)", r.kind)
	}
}

// lookupRegistered resolves a container.Handle to its registeredContainer
// under the handles map mutex, after checking ctx.Err(). The caller string
// is embedded in the error message so an "unknown handle" surfaces with the
// method name a caller expects (Exec / CaptureFiles / etc.).
//
// Destroy uses a similar but distinct pattern (it deletes-and-may-reinsert,
// which doesn't compose with this read-only helper).
//
// Slice 4.3 renamed/retyped from slice 4.2's lookupHandle (which returned a
// string) so callers can dispatch on r.kind. Image-mode callers extract
// r.dockerID inline; Task 4's compose-mode dispatch switches on r.kind.
func (b *Backend) lookupRegistered(ctx context.Context, caller string, h container.Handle) (registeredContainer, error) {
	if err := ctx.Err(); err != nil {
		return registeredContainer{}, err
	}
	b.mu.Lock()
	r, ok := b.handles[h.ID]
	b.mu.Unlock()
	if !ok {
		return registeredContainer{}, fmt.Errorf("container/docker: %s: unknown handle %q (not Created or already Destroyed)", caller, h.ID)
	}
	return r, nil
}

// ensureComposeCli lazy-initializes b.composeCli on first call, race-free via
// sync.Once. Cached error means a once-failed init doesn't retry. Image-mode
// Backend instances that never call createCompose pay zero cost.
//
// Task 3 ships the field + method but no caller yet — Task 4's createCompose
// is the first consumer.
func (b *Backend) ensureComposeCli() (command.Cli, error) {
	b.composeOnce.Do(func() {
		b.composeCli, b.composeErr = newComposeCli()
	})
	return b.composeCli, b.composeErr
}

// Close releases internal resources the Backend itself owns. Currently:
// the lazy composeCli's wrapped Docker client (constructed by
// command.NewDockerCli inside newComposeCli — separate from b.cli, which
// is owned by the caller of New() and must be closed by them).
//
// Without this, image-mode Backends with NO compose use leak nothing
// (composeCli never initialized), but compose-mode Backends leak the
// composeCli's HTTP transport goroutines on test teardown — surfaced by
// goleak.VerifyTestMain in main_test.go.
//
// Not part of the container.Backend interface (CLAUDE.md "seams as
// designed — no more"): docker is the only Backend with internal
// resources to release. Image-mode-only tests don't need to call Close;
// integ tests that use compose-mode AND production cli/backend.go's
// newBackend cleanup func both call it defensively.
//
// Idempotent; safe to call on a Backend that never created compose-mode
// state.
func (b *Backend) Close() error {
	if b.composeCli == nil {
		return nil
	}
	c := b.composeCli.Client()
	if c == nil {
		return nil
	}
	return c.Close()
}

// waitReady polls ContainerInspect until State.Health.Status == "healthy" IF
// the container has a healthcheck. Containers without a healthcheck return
// ready immediately (the entrypoint is responsible for readiness — spec §3).
//
// The deadline is derived from the image's HEALTHCHECK declaration:
//
//	deadline = StartPeriod + Interval × Retries × 1.5  (jitter buffer)
//	clamped to [30s, 5min]
//
// Rationale: docker daemon runs the healthcheck at the configured Interval;
// the worst-case time for a Status to flip to "healthy" or "unhealthy" is
// StartPeriod (initial grace) + Interval × Retries (consecutive checks).
// Adding 50% buffer covers daemon scheduling jitter. The 30s floor catches
// images with absurdly small intervals (e.g., 100ms); the 5min ceiling
// protects CI from hangs. (Authors who legitimately need >5min should raise
// the image's HEALTHCHECK Retries; a Backend-level override knob is a
// Phase 4 follow-up if a workload demands it.)
//
// The loop polls every waitReadyPollInterval using a ticker. The deadline is
// enforced atomically inside the select via time.NewTimer, so a slow Inspect
// call cannot exhaust the deadline before a single wait cycle has occurred.
func (b *Backend) waitReady(ctx context.Context, id string) error {
	info, err := b.cli.ContainerInspect(ctx, id)
	if err != nil {
		return fmt.Errorf("container/docker: waitReady: ContainerInspect: %w", err)
	}
	if info.State == nil || info.State.Health == nil {
		// No healthcheck declared → ready immediately.
		return nil
	}

	deadline := time.Now().Add(healthcheckDeadline(info))
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	ticker := time.NewTicker(waitReadyPollInterval)
	defer ticker.Stop()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	for {
		info, err := b.cli.ContainerInspect(ctx, id)
		if err != nil {
			return fmt.Errorf("container/docker: waitReady: ContainerInspect: %w", err)
		}
		if info.State == nil || info.State.Health == nil {
			// Daemon returned partial data — treat as still-starting and
			// continue polling. (The pre-loop nil-check already handled the
			// legitimate "no healthcheck declared" case.)
		} else {
			switch info.State.Health.Status {
			case "healthy":
				return nil
			case "unhealthy":
				return fmt.Errorf("container/docker: waitReady: container reported unhealthy")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			status := "<unknown>"
			if info.State != nil && info.State.Health != nil {
				status = info.State.Health.Status
			}
			return fmt.Errorf("container/docker: waitReady: timed out (status=%s)", status)
		case <-ticker.C:
		}
	}
}

// healthcheckDeadline derives the maximum wait time from the inspected
// container's HEALTHCHECK config. See waitReady's doc-comment for the
// formula. Inputs from info.Config.Healthcheck — nil info.Config or nil
// Healthcheck (e.g., daemon returned partial info, or a custom healthcheck
// was dropped post-create) falls back to waitReadyDefault.
//
// Package-level pure function (not a method on *Backend) — its output
// depends only on its inputs, which makes it trivially unit-testable
// without a *Backend instance, and signals to readers that no Backend
// state is consulted.
const (
	waitReadyFloor        = 30 * time.Second
	waitReadyCeiling      = 5 * time.Minute
	waitReadyDefault      = 60 * time.Second
	waitReadyPollInterval = 200 * time.Millisecond

	// nanoCPUsPerCPU is the Docker HostConfig.NanoCPUs unit per 1 vCPU.
	// Used by Create to translate ContainerResources.CPU (int vCPU count
	// as a string) into the NanoCPUs field the Docker daemon expects.
	nanoCPUsPerCPU = 1_000_000_000
)

func healthcheckDeadline(info dockerContainer.InspectResponse) time.Duration {
	if info.Config == nil || info.Config.Healthcheck == nil {
		return waitReadyDefault
	}
	hc := info.Config.Healthcheck
	retries := hc.Retries
	if retries < 1 {
		retries = 1
	}
	raw := hc.StartPeriod + hc.Interval*time.Duration(retries)
	buffered := raw + raw/2 // +50%
	if buffered < waitReadyFloor {
		return waitReadyFloor
	}
	if buffered > waitReadyCeiling {
		return waitReadyCeiling
	}
	return buffered
}

// Compile-time interface satisfaction.
var _ container.Backend = (*Backend)(nil)
