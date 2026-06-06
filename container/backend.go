// Package container provides the Backend seam — the interface the engine's
// Dispatcher uses to run commands inside long-lived containers (a single
// digest-pinned image or a compose project, per awf-workflow(5), CONTAINERS).
//
// Phase 2 ships only the interface and an in-memory fake (fake.go); Phase 4
// adds the Docker impl behind the same interface. Cross-impl conformance is
// the backendtest sub-package's job (RunBasicContract).
//
// Per runtime-design §3, container depends on no other AWF package.
// CaptureFiles therefore returns content bytes (CapturedFile{Path, Content}),
// NOT CAS refs (which would require Backend to Put to Blobs, violating the
// CLAUDE.md invariant "the interpreter is the only writer to state"). The
// slice 2.4 dispatcher passes the bytes up; the interpreter Puts at the
// commit boundary.
package container

import (
	"context"
	"errors"
)

// Backend executes commands inside long-lived containers. One concrete impl
// per phase: in-memory Fake (Phase 2, fake.go); Docker (Phase 4, docker.go).
//
// Lifecycle: Create on first reference for a declared container name, Exec /
// CaptureFiles many times across the run, Snapshot / Restore only for
// snapshot:workspace containers (Phase 4+; Phase 2 fake errors with
// ErrUnsupported), Destroy at run end or cancellation. The engine treats
// Handles as opaque; only the Backend that produced one may consume it.
//
// Concurrency: single-writer per instance in Phase 2 (matches
// state.InMemoryLog/InMemoryBlobs precedent; the interpreter is single-
// threaded for commits — runtime-design §5). Phase 3's `parallel` will need
// per-method synchronization on the fake — added when that slice lands, not
// pre-emptively here (CLAUDE.md rule 2: simplest solution first).
type Backend interface {
	// Capabilities reports what this backend supports. Must be callable on a
	// zero-state backend (no Create needed); the engine queries it at startup
	// to decide whether snapshot:workspace containers can be served.
	Capabilities() Caps

	// Create brings a container to readiness per the spec. The returned Handle
	// is opaque to the caller and must be passed back to Exec / CaptureFiles /
	// Snapshot / Destroy. Phase 2 fake: allocates an in-mem fs map keyed by a
	// monotonic ID. Phase 4 Docker: image-pinned create or compose `up --wait`.
	Create(ctx context.Context, spec ContainerSpec) (Handle, error)

	// Exec runs a command synchronously inside the handle. Returns two
	// channels and an error.
	//
	// chunks: STREAMING channel of IOChunks. Emits each stdout/stderr chunk
	//   as it arrives from the underlying pipe — callers ranging over it
	//   observe the process's output in real time. Closed by the Backend
	//   implementation when both stdout and stderr pipes drain (process
	//   either exited or ctx-cancelled). MUST be drained by the caller; an
	//   unread chunk back-pressures the reader goroutines (chan buffer is
	//   unspecified but bounded).
	//
	// result: SINGLE-VALUE channel of ExecResult. Receives exactly one
	//   value AFTER the chunks channel closes AND the process has been
	//   waited-on (ExitCode + accumulated Stdout + Err available). Then
	//   closes. Receiving from it before chunks closes is permitted (blocks
	//   until the close) but unusual — the natural order is
	//   `for range chunks { ... }; r := <-result`. ExecResult.Err surfaces
	//   transport-class errors (stdcopy mid-stream failure on docker;
	//   ctx.Canceled on native pipe close mid-read) WITHOUT swallowing them
	//   silently.
	//
	// On non-nil error return, BOTH channels are nil; callers must check
	// err before ranging or receiving. The "launch / transport" error
	// class — couldn't start the command at all — surfaces here. If the
	// command ran and merely exited nonzero, err is nil and result's
	// ExecResult.ExitCode carries the code.
	//
	// ctx cancellation: aborts the command; chunks channel closes; result
	// emits an ExecResult with Err set to ctx.Err() (and likely
	// process-killed ExitCode). The function's error return remains nil —
	// the cancellation surfaces via the result value's Err field, NOT via
	// err. Callers that used `errors.Is(err, context.Canceled)` must
	// update to check `result.Err` instead.
	//
	// Slice 5.3: STREAMING refactor of the original Phase 2
	// "(ExecResult, <-chan IOChunk, error)" contract. The Claude Code
	// adapter (agent/claude.Launch) needs progressive event arrival;
	// refactoring Backend.Exec across all three backends is the
	// load-bearing prerequisite.
	Exec(ctx context.Context, h Handle, cmd Cmd) (<-chan IOChunk, <-chan ExecResult, error)

	// CaptureFiles reads the named in-container paths and returns their content,
	// one CapturedFile per path in the input order. Missing-path is a hard error
	// (no partial returns) — output_files are declared by the author and absence
	// at commit time is a step bug, not graceful degradation. The slice 2.4
	// dispatcher passes the bytes up; the interpreter Puts each into Blobs at
	// the commit boundary (this method never touches state.Blobs; see pkg doc).
	CaptureFiles(ctx context.Context, h Handle, paths []string) ([]CapturedFile, error)

	// CopyTo writes each InputFile's bytes to its in-container path BEFORE the
	// step's Exec/Launch — the symmetric inverse of CaptureFiles. Overwrites an
	// existing path. len(files)==0 is a no-op returning nil. Unknown handle is a
	// hard error. Never touches state.Blobs.
	CopyTo(ctx context.Context, h Handle, files []InputFile) error

	// Snapshot captures the handle's filesystem as a CoW diff. Only meaningful
	// for snapshot:workspace containers (spec §3). Phase 2 fake: returns
	// ("", ErrUnsupported). Phase 4 Docker (slice 4.4): streaming gzip-tar
	// diff via ContainerDiff + per-path CopyFromContainer through
	// state.Blobs.Put. Returns a 3-segment SnapshotRef:
	//     <state-blobs-ref>@<image-ref>@<base64-json-of-cmd-entrypoint>
	// so Restore can ContainerCreate against the source image AND faithfully
	// re-apply the effective Cmd/Entrypoint (a Restore→Snapshot loop
	// preserves runtime config across the round-trip).
	//
	// Peak memory: bounded by snapshotMaxBlobBytes at state.Blobs.Put time
	// (the compressed blob lives in memory for the Put call; the streaming
	// gzip+tar intermediate buffers stay at ~64 KiB throughout the build).
	//
	// If the gzip-compressed diff exceeds the configured cap (default 256
	// MiB), returns *ErrSnapshotTooLarge — the engine routes this as
	// permanent_failure.
	Snapshot(ctx context.Context, h Handle) (SnapshotRef, error)

	// Restore rebuilds a handle from a prior Snapshot. The name argument is the
	// IR-declared container name (spec §3); the returned Handle.Name is set to
	// it so the dispatcher's d.Handles[name] keys consistently across
	// pre-snapshot Create and post-resume Restore.
	//
	// Phase 2 fake: returns ErrUnsupported.
	//
	// Phase 4 Docker (slice 4.4): parses the 3-segment SnapshotRef, Blobs.Get
	// the diff-tar, ContainerCreate against the embedded image + Cmd +
	// Entrypoint, stream-CopyToContainer the data entries via io.Pipe (peak
	// memory ~64 KiB + the diff blob bytes already in RAM from Get),
	// Exec "rm -rf -- '<path>'" for each .awf-deletes entry. waitReady runs
	// on the restored container so the spec §3 readiness contract holds.
	// The embedded image is NOT auto-pulled; callers responsible for prior
	// ImagePull (same as Backend.Create's image-mode path).
	Restore(ctx context.Context, ref SnapshotRef, name string) (Handle, error)

	// Destroy releases the handle's resources. The caller must call Destroy
	// exactly once per Create; a second call returns an error (the handle is
	// gone). Matches os.File.Close and Docker's ContainerRemove behavior;
	// io.Closer itself leaves post-first-call behavior undefined, but the
	// engine relies on the stricter "error on double" guarantee to surface
	// dispatcher bugs.
	Destroy(ctx context.Context, h Handle) error
}

// Caps reports a backend's capabilities. Backends MUST explicitly construct
// (e.g. Caps{Snapshot: SnapshotNone}) — zero-value Caps has Snapshot == ""
// so a missing assignment is observable in tests rather than silently
// advertising "no snapshot."
type Caps struct {
	Snapshot SnapshotMode

	// RuntimeImage reports whether the backend can honor a map's runtime-
	// resolved per-element image: (P6a) — i.e. actually boot the rendered
	// image and report its content digest. Zero-value false FAILS CLOSED:
	// a backend that ignores image: (native) advertises false, and the CLI
	// guard rejects a runtime-image workflow on it rather than silently
	// running bodies on the host.
	RuntimeImage bool
}

// SnapshotMode is the snapshot capability a backend supports.
type SnapshotMode string

const (
	// SnapshotNone — the backend has no snapshot facility. Phase 2 fake; any
	// non-snapshot:workspace Docker container in Phase 4.
	SnapshotNone SnapshotMode = "none"
	// SnapshotFSCoW — the backend can capture and restore a CoW filesystem
	// diff. Phase 4 Docker for snapshot:workspace containers.
	SnapshotFSCoW SnapshotMode = "fs-cow"
)

// ContainerSpec describes a container the engine wants Created. The Backend
// dispatches on which mode-specific fields are populated:
//
//   - Image-mode (slice 4.1): Image required, Resources optional, Cmd
//     optional, Compose nil.
//   - Compose-mode (slice 4.3): Compose+ComposePath+Service required,
//     Image empty, Resources/Cmd nil (per-service config lives in the
//     compose file).
//
// The Phase 2 fake ignores every field except Name.
type ContainerSpec struct {
	Name string

	// Image-mode fields.
	Image     string
	Resources *ContainerResources
	// Cmd is an optional override for the image's CMD instruction. When
	// nil or empty, the image's default Cmd applies. Slice 4.4 adds this
	// field so test fixtures can inject a long-running entrypoint into
	// short-CMD images (e.g., alpine's /bin/sh → sleep infinity) without
	// bypassing Backend.Create. Today's engine.ContainerSpecFor never
	// populates Cmd; a future IR slice adding `cmd: [...]` to Container
	// declarations would.
	Cmd []string

	// PullIfAbsent requests that Create ensure Image is present in the local
	// cache before starting it — image-mode only (P6a). The engine sets it
	// solely for a map's runtime-resolved per-element image: (engine/map.go),
	// where the image is learned at dispatch and is generally not pre-pulled.
	// Statically-declared containers leave it false: they are pre-provisioned
	// from a validator-pinned (digest) image, so the run-start pre-pull suffices.
	//
	// Because a runtime-resolved image cannot be validator-pinned, a backend
	// honoring this flag MUST require Image to be digest-pinned (name@sha256:…)
	// so the booted bytes are content-addressed and resume-reproducible, and
	// MUST report the booted digest on Handle.ResolvedImageDigest. The Docker
	// backend implements this; backends that ignore image: (native) advertise
	// Caps.RuntimeImage=false and are rejected by the CLI guard before dispatch.
	PullIfAbsent bool

	// Compose-mode fields (slice 4.3).
	Compose     []byte
	ComposePath string
	Service     string
}

// ContainerResources mirrors the IR Container.Resources fields (spec §3) for
// transport to the Backend. Image-mode only — for compose-mode, per-service
// resources are declared in the compose file.
type ContainerResources struct {
	CPU string
	Mem string
}

// Handle identifies a Created container. Treated as opaque by the engine;
// only the Backend that produced it may consume it.
//
// Service is non-empty iff the handle is compose-shaped — it's the service
// the next Exec call will route to. The engine dispatcher (engine/local_dispatcher.go
// runCode) MAY override Service by cloning the Handle value before passing
// to Exec — this is the spec §3 `container: lab:db` cross-service form.
type Handle struct {
	Name    string
	ID      string
	Service string

	// ResolvedImageDigest is the content digest of the image that actually
	// booted (P6a) — set by Create when the image was runtime-resolved (a map's
	// per-element image:). Empty for statically-pinned containers and backends
	// that don't resolve a digest (native). The engine records it on the
	// element's map.item commit so resume has a durable record of what booted.
	ResolvedImageDigest string
}

// Cmd describes a command to Exec. Run is the shell command (from CodeStep.Run
// after template substitution). Env carries the dispatcher-injected env vars
// (slice 2.4: AWF_IDEMPOTENCY_KEY; slice 4.2: AWF_OUTPUT when output_schema is
// declared); the Phase 2 fake accepts Env but does not use it (its scripted
// result table is keyed on Run alone — slice 2.4 verifies dispatcher env
// injection by inspecting what it passes to Backend.Exec, not by what the fake
// does with the env it receives).
//
// Shell-quoting note: Run is interpreted as a single string passed to `sh -c`
// (POSIX baseline) — slice 4.2 implementation in container/docker/exec.go.
// Authors needing bash-specific features (`<(...)`, `[[ ]]`, double-bracket
// regex) ship bash in their image and write `bash -c '...'` as the inner
// script. An author who templates an untrusted agent-controlled value into Run
// MUST quote the substitution: `./scan.sh "{{ step.x.url }}"` — an unquoted
// `$(...)` / backtick / `;` in the typed value becomes an unintended command.
// Phase 4 may add a parallel `Argv []string` field for shell-free exec if a
// workload requires it; Phase 2 takes the simpler path and documents the
// contract.
type Cmd struct {
	Run string
	Env map[string]string
}

// ExecResult is the streamed result of an Exec call (slice 5.3: delivered
// via Backend.Exec's result channel). Slice 2.4's outcome classifier reads
// ExitCode (per §6 — nonzero outside non_retryable_exit_codes is
// retryable_failure; in the list is permanent_failure); the dispatcher
// parses AWFOutput against the step's output_schema for typed outputs;
// Stdout is exposed via {{ step.<id>.stdout }} substitution.
//
// Stderr is intentionally NOT a field — awf-workflow(5) (Code step) lists only
// exit_code and stdout as implicit outputs; the templating section never references stderr. The
// live tap (slice 2.5) and node.failed events (slice 2.4) get stderr via
// IOChunk{Stream: "stderr"} on the chunk channel.
type ExecResult struct {
	ExitCode  int
	AWFOutput []byte
	Stdout    []byte
	// Err (slice 5.3) surfaces transport-class errors that occurred AFTER
	// Backend.Exec successfully launched the process — stdcopy mid-stream
	// failure (docker), pipe-read failure (native), ctx-cancel during
	// in-flight read. nil on the happy path. Pre-slice-5.3 these came back
	// via Backend.Exec's err return; the streaming refactor moved them
	// here so the result channel can carry them without the function
	// returning early before chunks drain.
	Err error
}

// IOChunk is one stream slice produced during Exec. Phase 2 emits the chunks
// queued by the fake's ProgramExec; Phase 4 Docker forwards real stream
// frames. The Stream field is "stdout" or "stderr" — typed loosely so future
// streams (e.g. agent.event) can reuse the type without churn.
type IOChunk struct {
	Stream string
	Data   []byte
}

// CapturedFile is one path + its contents, as returned by CaptureFiles. The
// slice 2.4 dispatcher passes them up; the interpreter Puts each Content into
// Blobs at the commit boundary and records Path → ref in
// engine.NodeCompletedData.Files.
type CapturedFile struct {
	Path    string
	Content []byte
}

// InputFile is one in-container destination path + the bytes to write there,
// the symmetric input to CopyTo. The engine (the only Blobs reader) resolves an
// input_files ref to a CAS blob, Blobs.Get's the bytes, and passes them here.
// This method never touches state.Blobs (see pkg doc).
type InputFile struct {
	Path    string
	Content []byte
}

// SnapshotRef is the opaque reference returned by Snapshot and consumed by
// Restore. Phase 2 fake never produces one (Snapshot returns ErrUnsupported);
// Phase 4 sets it to the CAS ref of the CoW diff.
type SnapshotRef string

// ErrUnsupported is the sentinel returned by Backend methods this backend
// doesn't implement. Phase 2 fake returns it from Snapshot / Restore. Callers
// route with errors.Is(err, container.ErrUnsupported).
var ErrUnsupported = errors.New("container: operation not supported by this backend")

// ErrSnapshotTooLarge is the sentinel for a deterministic snapshot-capacity
// failure (the workspace diff exceeds the backend's size cap). A backend's
// concrete too-large error wraps this so callers can errors.Is without
// importing the concrete backend package. Phase-4 decision 11: this is a
// permanent_failure (retrying re-runs the step to fail identically).
var ErrSnapshotTooLarge = errors.New("snapshot diff exceeds the backend size limit")
