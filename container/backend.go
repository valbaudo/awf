// Package container provides the Backend seam — the interface the engine's
// Dispatcher uses to run commands inside long-lived containers (a single
// digest-pinned image or a compose project, per AgentWorkflowFormat.md §3).
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

	// Exec runs a command synchronously inside the handle and returns its full
	// result (ExitCode, AWFOutput, Stdout). The returned channel is buffered
	// and CLOSED before Exec returns — it contains every IOChunk the command
	// produced. The synchronous-with-pre-buffered-chunks shape lets Phase 2
	// ship without spawning goroutines; Phase 4 may revisit if Docker's
	// streaming API requires true live consumption (the receive-only direction
	// keeps both impls compatible). ctx cancellation aborts the command; the
	// returned error is non-nil if Exec couldn't run the command at all (the
	// "launch / transport" class — retryable_failure per §6); if the command
	// ran and merely exited nonzero, err is nil and ExitCode carries the code.
	//
	// On non-nil error, the returned channel is nil; callers MUST check err
	// before ranging over the channel (a `for range nil-chan` deadlocks).
	Exec(ctx context.Context, h Handle, cmd Cmd) (ExecResult, <-chan IOChunk, error)

	// CaptureFiles reads the named in-container paths and returns their content,
	// one CapturedFile per path in the input order. Missing-path is a hard error
	// (no partial returns) — output_files are declared by the author and absence
	// at commit time is a step bug, not graceful degradation. The slice 2.4
	// dispatcher passes the bytes up; the interpreter Puts each into Blobs at
	// the commit boundary (this method never touches state.Blobs; see pkg doc).
	CaptureFiles(ctx context.Context, h Handle, paths []string) ([]CapturedFile, error)

	// Snapshot captures the handle's filesystem as a CoW diff. Only meaningful
	// for snapshot:workspace containers (spec §3). Phase 2 fake: returns
	// ("", ErrUnsupported). Phase 4 adds the real impl.
	Snapshot(ctx context.Context, h Handle) (SnapshotRef, error)

	// Restore rebuilds a handle from a prior Snapshot. Same Phase-2-fake
	// disposition as Snapshot.
	Restore(ctx context.Context, ref SnapshotRef) (Handle, error)

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
//   - Image-mode (slice 4.1): Image required, Resources optional, Compose nil.
//   - Compose-mode (slice 4.3): Compose+ComposePath+Service required, Image
//     empty, Resources nil (per-service resources live in the compose file).
//
// The Phase 2 fake ignores every field except Name — its scripted Exec table
// is keyed on Cmd.Run alone, and Create returns a Handle{Service: ""} for any
// spec shape (matches the image-mode equivalence for the fake's purposes).
type ContainerSpec struct {
	Name string

	// Image-mode fields (slice 4.1).
	Image     string
	Resources *ContainerResources

	// Compose-mode fields (slice 4.3). Compose bytes flow from
	// ir.LoadedDefinition.ComposeFiles (validator already digest-pinned them);
	// ComposePath is the workflow-relative path (compose-go filename hint);
	// Service is the default service from IR §3 `service:` (steps exec into it
	// unless they override via `container: lab:db`).
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

// ExecResult is the synchronous result of an Exec call. Slice 2.4's outcome
// classifier reads ExitCode (per §6 — nonzero outside non_retryable_exit_codes
// is retryable_failure; in the list is permanent_failure); the dispatcher
// parses AWFOutput against the step's output_schema for typed outputs; Stdout
// is exposed via {{ step.<id>.stdout }} substitution.
//
// Stderr is intentionally NOT a field — AgentWorkflowFormat §4.1 lists only
// exit_code and stdout as implicit outputs; §7 never templates stderr. The
// live tap (slice 2.5) and node.failed events (slice 2.4) get stderr via
// IOChunk{Stream: "stderr"} on the chunk channel.
type ExecResult struct {
	ExitCode  int
	AWFOutput []byte
	Stdout    []byte
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

// SnapshotRef is the opaque reference returned by Snapshot and consumed by
// Restore. Phase 2 fake never produces one (Snapshot returns ErrUnsupported);
// Phase 4 sets it to the CAS ref of the CoW diff.
type SnapshotRef string

// ErrUnsupported is the sentinel returned by Backend methods this backend
// doesn't implement. Phase 2 fake returns it from Snapshot / Restore. Callers
// route with errors.Is(err, container.ErrUnsupported).
var ErrUnsupported = errors.New("container: operation not supported by this backend")
