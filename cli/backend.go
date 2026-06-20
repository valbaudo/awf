package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	dockerclient "github.com/docker/docker/client"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/docker"
	"github.com/valbaudo/awf/container/native"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// newBackend constructs a container.Backend for a single CLI invocation
// (production path; tests inject Runner.Backend directly and skip this).
// The cleanup func is deferred by the subcommand AFTER the per-handle
// Destroy loop — closing the underlying Docker client before Destroy fires
// would break teardown.
//
// kind is one of engine.BackendFake / engine.BackendDocker / engine.BackendNative;
// any other value (including "") is an error. The caller (cli/resume.go via
// readBackendKindFromLog) is responsible for mapping legacy "" → docker
// BEFORE invoking newBackend; an empty kind reaching newBackend would be
// a programming error caught by the default-arm "unknown backend kind"
// error path.
//
// workdirRoot is consumed only by the native arm (path under which
// per-container workdirs are created). Other arms ignore it.
//
// stderr is used only by the native arm to emit the no-sandbox isolation
// warning (WS-5); other arms ignore it.
//
// Private — no exported type, no Runner field, no plugin layer (CLAUDE.md
// "seams as designed — no more").
func newBackend(ctx context.Context, kind, runID, workdirRoot string, blobs state.Blobs, stderr io.Writer) (container.Backend, func(), error) {
	_ = ctx // reserved for future hooks (e.g. client.Ping); structural for now per slice-4.5 plan.
	switch kind {
	case engine.BackendFake:
		return container.NewFake(), func() {}, nil
	case engine.BackendNative:
		if blobs == nil {
			panic("cli: newBackend native: blobs must not be nil") // OpenBlobs never returns nil-without-error; callers exit first
		}
		b, err := native.New(workdirRoot, native.WithBlobs(blobs), native.WithSandbox(true))
		if err != nil {
			return nil, nil, fmt.Errorf("cli: construct native backend: %w", err)
		}
		// Emit the no-sandbox isolation warning when no platform sandbox was
		// available. Mirrors the no-image warning at cli/run.go:354.
		if label := b.SandboxWarnLabel(); label != "" {
			fprintf(stderr, "awf run: --backend native: %s\n", label)
		}
		return b, func() {}, nil
	case engine.BackendDocker:
		cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
		if err != nil {
			return nil, nil, fmt.Errorf("cli: construct docker client: %w", err)
		}
		b, err := docker.New(cli, runID, blobs)
		if err != nil {
			_ = cli.Close()
			return nil, nil, fmt.Errorf("cli: construct docker backend: %w", err)
		}
		// Cleanup closes BOTH the externally-owned dockerclient AND the
		// internally-owned composeCli's wrapped client. Without b.Close(),
		// compose-mode Backends leak HTTP transport goroutines (goleak
		// detects them in CI).
		return b, func() {
			_ = b.Close()
			_ = cli.Close()
		}, nil
	default:
		// Catches "" (caller bug — readBackendKindFromLog defaults legacy
		// empties to docker before calling here) AND unknown kinds.
		return nil, nil, fmt.Errorf("cli: unknown backend kind %q (want %q, %q, or %q)", kind, engine.BackendFake, engine.BackendDocker, engine.BackendNative)
	}
}

// resolveBackend returns r.Backend when non-nil (test-injection path) or
// constructs a fresh Backend via newBackend (production path). The returned
// cleanup func is a no-op when r.Backend was injected; the caller defers it
// unconditionally and the closure-vs-Destroy ordering is the same in both
// cases (cleanup fires LIFO AFTER the per-handle Destroy loop).
//
// stderr is forwarded to newBackend for the native arm's no-sandbox warning
// (WS-5); it is unused when r.Backend is injected (test path).
//
// Centralized here so cli/run.go (kind from --backend flag) and
// cli/resume.go (kind from run.started.Backend) share one dispatch.
func (r *Runner) resolveBackend(ctx context.Context, kind, runID, workdirRoot string, blobs state.Blobs, stderr io.Writer) (container.Backend, func(), error) {
	if r.Backend != nil {
		return r.Backend, func() {}, nil
	}
	return newBackend(ctx, kind, runID, workdirRoot, blobs, stderr)
}

// readBackendKindFromLog extracts the Backend kind from a folded log's
// run.started event. Returns BackendDocker for a missing-or-empty Backend
// field (pre-slice-4.5 legacy log default). Returns an error for an
// unknown kind value, or if no run.started event exists in the slice
// (defensive — run.started is always the first event of a well-formed log).
//
// Pure function over the events slice; unit-testable without Backend
// construction or workflow file. cli/resume.go is the only production
// caller; cli/backend_test.go invokes it directly via the export_test
// alias.
func readBackendKindFromLog(events []state.Event) (string, error) {
	for _, e := range events {
		if e.Type == engine.EventRunStarted {
			var d engine.RunStartedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return "", fmt.Errorf("cli: unmarshal run.started: %w", err)
			}
			kind := d.Backend
			if kind == "" {
				kind = engine.BackendDocker
			}
			switch kind {
			case engine.BackendNative:
				return kind, nil
			case backendAuto:
				return "", fmt.Errorf("cli: run log records unresolved backend %q", backendAuto)
			case engine.BackendFake, engine.BackendDocker:
				return kind, nil
			default:
				return "", fmt.Errorf("cli: log contains unknown backend kind %q in run.started (this binary supports %q, %q, or %q)", kind, engine.BackendFake, engine.BackendDocker, engine.BackendNative)
			}
		}
	}
	return "", fmt.Errorf("cli: no run.started event in log (malformed)")
}
