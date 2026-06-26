package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// blobStore is a minimal CAS interface so container/fake stays free of any AWF
// package import (container depends on no other AWF package — see backend.go
// pkg doc). state.Blobs / state.InMemoryBlobs satisfy it structurally.
type blobStore interface {
	Put([]byte) (string, error)
	Get(string) ([]byte, error)
}

// RestoreCall records one Restore invocation (test assertion aid).
type RestoreCall struct {
	Name string
	Ref  SnapshotRef
}

// WriteFileAtCall records a WriteFileAt for test assertions (mirrors RestoreCall).
type WriteFileAtCall struct {
	Path    string
	Content []byte
}

// Fake is the in-memory Backend used by Phase 2 engine tests and the
// conformance suite (slice 2.6). Deterministic: monotonic-counter handle
// IDs, no time.Now, no OS-level process spawning, no goroutines.
//
// Thread-safe (Phase 3 slice 3.2): per-method mutex protects handles /
// execTable / streamTable / Calls / counters so parallel branch
// goroutines dispatching to distinct containers (all backed by the same
// *Fake via the harness's Handles map) can race-cleanly.
//
// Phase 4 Docker impl will live alongside this in container/docker.go;
// backendtest.RunBasicContract runs against both unchanged.
type Fake struct {
	mu sync.Mutex

	handles map[string]*fakeHandle
	nextID  int

	// Scripted Exec table — keyed on Cmd.Run.
	execTable   map[string]ExecResult
	streamTable map[string][]IOChunk

	// fileTable, keyed on Cmd.Run, records files a programmed command WRITES into
	// the executing handle's fs when it runs — simulating output_files production
	// on the scripted fake (SP1 Task 8a). Set via ProgramExecWithFiles; consulted
	// in Exec under the same mutex, BEFORE the dispatcher's post-Exec CaptureFiles.
	fileTable map[string]map[string][]byte

	// Fault hooks — set via FailExecAfterN / FailCaptureAfterN (Task 3).
	// nil = no fault. The check in Exec/CaptureFiles is wired now so Task 3
	// is a single setter addition rather than threading code through here.
	execCalls    int
	captureCalls int
	failExecAt   *int
	failCapAt    *int

	// blockExecCh, when non-nil, is a gate channel that each Exec call blocks
	// on (AFTER recording the call and BEFORE the fault/table checks). Closed
	// by the test via ReleaseBlockedExec to allow Exec to proceed. Used by
	// signal conformance tests to ensure the pollControls goroutine fires
	// before any step completes (timing-sensitive pause/cancel assertions).
	blockExecCh chan struct{}

	// Calls is the defensive-copied history of every Cmd this fake's Exec
	// received. Slice 2.4 (and later) tests inspect this to verify the
	// dispatcher's env-injection contract (AWF_IDEMPOTENCY_KEY per AWF §10).
	// The fake's doc-comment anticipated this need at slice-2.2 time but
	// shipped no mechanism; this is the recording slot.
	Calls []Cmd

	// CreateSpecs records every ContainerSpec passed to Create, in order
	// (test assertion aid — mirrors Calls for Exec). The P6a wiring test reads
	// PullIfAbsent / Image off it to verify the engine flags a map's
	// runtime-resolved image spec for the backend to pull. Shallow copies: the
	// recorded scalars (Name/Image/PullIfAbsent) are what tests assert on.
	CreateSpecs []ContainerSpec

	// ExecHandles records the Handle value passed to each Exec call. Tests use
	// this to assert compose service overrides (`container: lab:api`) reached
	// the backend as a handle-level Service override.
	ExecHandles []Handle

	// DestroyCalls records every Handle passed to Destroy, in order.
	DestroyCalls []Handle

	// "Any" programmed response (slice 5.3 Task 16). Used by tests where
	// the Cmd.Run is built by the caller (the test's SUBJECT), not the
	// key to look up. nil = unset; takes effect only as a fall-through
	// after execTable[cmd.Run] misses.
	anyExec   *ExecResult
	anyChunks []IOChunk

	// blobs, when non-nil (set via WithBlobs), is the CAS store the fake
	// serializes a container's in-mem fs into on Snapshot and reads back on
	// Restore — the durable path that survives the conformance harness's
	// run→resume fake recreation (slice 7.1). nil = SnapshotNone / ErrUnsupported.
	blobs blobStore

	// RestoreCalls records every Restore invocation, in order (test assertion
	// aid — mirrors Calls for Exec).
	RestoreCalls []RestoreCall

	// WriteFileAtCalls records every WriteFileAt invocation, in order (test
	// assertion aid — mirrors RestoreCalls).
	WriteFileAtCalls []WriteFileAtCall

	// P6a — programmable spec.Image → resolved-digest table; Create returns the
	// looked-up digest on the Handle ("" if unprogrammed). failCreate models an
	// unavailable runtime image (Create errors).
	imageDigests     map[string]string
	failCreate       map[string]bool
	failCreateConfig map[string]bool
}

// fakeHandle is the per-Create internal state: an in-mem fs map.
type fakeHandle struct {
	files map[string][]byte
}

// NewFake mints an empty Fake. Returns *Fake (not Backend) so test callers
// can reach helpers like ProgramExec / WriteFile / FailExecAfterN that the
// Backend interface deliberately doesn't expose.
func NewFake() *Fake {
	return &Fake{handles: map[string]*fakeHandle{}}
}

// ProgramImageDigest maps a spec.Image value to the resolved content digest the
// next Create against it returns on Handle.ResolvedImageDigest (P6a test helper;
// not on the Backend interface). Lazily allocates the table.
func (f *Fake) ProgramImageDigest(image, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.imageDigests == nil {
		f.imageDigests = map[string]string{}
	}
	f.imageDigests[image] = digest
}

// FailCreateForImage makes Create return a *container.ImageUnavailableError for
// a spec.Image — models a valid runtime image that can't be pulled/booted (the
// SOLE tolerated per-element Create failure, P6a). Lazily allocates the set.
func (f *Fake) FailCreateForImage(image string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate == nil {
		f.failCreate = map[string]bool{}
	}
	f.failCreate[image] = true
}

// FailCreateConfigForImage makes Create return a PLAIN (non-ImageUnavailable)
// error for a spec.Image — models a deterministic DEFINITION fault (e.g. a
// malformed resources: the backend rejects) that the engine must surface by
// failing the WHOLE map as permanent_failure, NOT tolerate under min_success
// (contrast FailCreateForImage, the tolerated availability failure). Lazily
// allocates the set.
func (f *Fake) FailCreateConfigForImage(image string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreateConfig == nil {
		f.failCreateConfig = map[string]bool{}
	}
	f.failCreateConfig[image] = true
}

// WithBlobs wires a CAS store so the fake can serialize a container's in-mem
// filesystem into Blobs on Snapshot and read it back on Restore — the durable
// path that survives the run→resume fake recreation (the conformance harness
// mints a fresh Fake for run and another for resume, but the same state.Blobs
// survives). Without it the fake advertises SnapshotNone and Snapshot/Restore
// return ErrUnsupported (preserving the Phase 2 default). Returns f for chaining.
func (f *Fake) WithBlobs(b blobStore) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs = b
	return f
}

func (f *Fake) Capabilities() Caps {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The fake resolves runtime images via its programmable digest table, so it
	// advertises RuntimeImage (P6a). Snapshot still depends on an injected CAS.
	// StagingRoot mirrors Docker: "/work/.awf" (the Fake is a Docker-equivalent
	// in-mem backend; tests that simulate native override this via a thin wrapper).
	if f.blobs != nil {
		return Caps{Snapshot: SnapshotFSCoW, RuntimeImage: true, RuntimeCompose: true, StagingRoot: "/work/.awf"}
	}
	return Caps{Snapshot: SnapshotNone, RuntimeImage: true, RuntimeCompose: true, StagingRoot: "/work/.awf"}
}

func (f *Fake) Create(_ context.Context, spec ContainerSpec) (Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Record BEFORE the fault check (mirrors Exec/Calls): a Create was requested
	// with this spec, so a test observes it even if the lookup fails it.
	f.CreateSpecs = append(f.CreateSpecs, spec)
	if f.failCreate[spec.Image] {
		// A tolerated availability/boot failure (P6a): the engine routes the typed
		// error to item_failed + ReasonImageUnavailable, counted against min_success.
		return Handle{}, &ImageUnavailableError{Image: spec.Image, Err: errors.New("programmed unavailable")}
	}
	if f.failCreateConfig[spec.Image] {
		// A deterministic definition fault: a PLAIN error the engine must NOT
		// tolerate — it fails the whole map as permanent_failure.
		return Handle{}, fmt.Errorf("container/fake: Create: image %q programmed config-invalid", spec.Image)
	}
	f.nextID++
	id := fmt.Sprintf("fake-%d", f.nextID)
	f.handles[id] = &fakeHandle{files: map[string][]byte{}}
	return Handle{Name: spec.Name, ID: id, Service: spec.Service, ResolvedImageDigest: f.imageDigests[spec.Image]}, nil
}

// Exec returns the ExecResult programmed for cmd.Run (via ProgramExec). The
// streaming contract (slice 5.3): returns two channels. chunks is buffered
// with every programmed IOChunk and pre-closed; result is 1-buffered with the
// programmed ExecResult and pre-closed. chunks closes BEFORE result emits
// (the Fake's deterministic-burst semantic — every chunk is materialized
// before the result is observable).
//
// Unprogrammed cmd.Run is a hard error — silent zero-value would mask
// dispatcher bugs in slice 2.4. Unknown handle is a hard error. Cmd.Env is
// accepted but not used (slice 2.4 verifies dispatcher env injection by
// inspecting what it passes to Exec, not by what the fake does with it).
//
// On any error return, both channels are nil — the new Backend contract
// requires callers to check err before ranging or receiving.
func (f *Fake) Exec(ctx context.Context, h Handle, cmd Cmd) (<-chan IOChunk, <-chan ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.handles[h.ID]; !ok {
		return nil, nil, fmt.Errorf("container/fake: Exec: unknown handle %q", h.ID)
	}
	// Record the call BEFORE the fault check / programmed lookup — the dispatcher
	// invoked us with this Cmd; the recording is what the env-injection test
	// asserts on regardless of whether the lookup succeeds.
	f.Calls = append(f.Calls, cloneCmd(cmd))
	f.ExecHandles = append(f.ExecHandles, h)

	// Block gate: if the test armed a blockExecCh, release the mutex and wait
	// until the channel is closed (or ctx is cancelled). This lets the caller
	// write a pause/cancel control file and give the pollControls goroutine a
	// full scheduler turn before any step completes. Conformance Bucket 8
	// (signal pause/cancel) is the primary user.
	blockCh := f.blockExecCh
	if blockCh != nil {
		f.mu.Unlock()
		select {
		case <-blockCh:
		case <-ctx.Done():
			f.mu.Lock()
			return nil, nil, ctx.Err()
		}
		f.mu.Lock()
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
	}

	if f.failExecAt != nil && f.execCalls == *f.failExecAt {
		n := f.execCalls
		f.execCalls++
		return nil, nil, fmt.Errorf("container/fake: induced Exec fault at call #%d", n)
	}
	f.execCalls++

	programmed, ok := f.execTable[cmd.Run]
	streamed := f.streamTable[cmd.Run] // may be nil
	if !ok {
		if f.anyExec != nil {
			programmed = *f.anyExec
			streamed = f.anyChunks
		} else {
			return nil, nil, fmt.Errorf("container/fake: Exec: no programmed result for cmd.Run=%q (call ProgramExec first)", cmd.Run)
		}
	}

	// Write any files this programmed command produces into the handle's fs
	// (SP1 Task 8a) — under the same mutex, BEFORE returning, so the
	// dispatcher's post-Exec CaptureFiles (output_files capture) finds them.
	// fh is the executing handle (existence verified above).
	if produced := f.fileTable[cmd.Run]; produced != nil {
		fh := f.handles[h.ID]
		for p, b := range produced {
			fh.files[p] = cloneBytes(b)
		}
	}

	// Build the two channels. chunks is pre-buffered (deterministic burst is
	// the Fake's contract); result is 1-buffered. Both are pre-closed so the
	// caller observes the full burst plus the result without blocking.
	chunks := make(chan IOChunk, len(streamed))
	for _, c := range streamed {
		chunks <- c
	}
	close(chunks)
	result := make(chan ExecResult, 1)
	result <- programmed
	close(result)
	return chunks, result, nil
}

// CaptureFiles reads each path from the handle's in-mem fs and returns the
// content in input order. Missing-path errors the whole call; unknown handle
// is a hard error. Defensive-copies content (matches state.InMemoryBlobs.Get
// discipline — callers mutating their slice must not corrupt our store).
func (f *Fake) CaptureFiles(ctx context.Context, h Handle, paths []string) ([]CapturedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, ok := f.handles[h.ID]
	if !ok {
		return nil, fmt.Errorf("container/fake: CaptureFiles: unknown handle %q", h.ID)
	}
	if f.failCapAt != nil && f.captureCalls == *f.failCapAt {
		n := f.captureCalls
		f.captureCalls++
		return nil, fmt.Errorf("container/fake: induced CaptureFiles fault at call #%d", n)
	}
	f.captureCalls++

	out := make([]CapturedFile, 0, len(paths))
	for _, p := range paths {
		content, ok := fh.files[p]
		if !ok {
			return nil, fmt.Errorf("container/fake: CaptureFiles: path %q not present in handle %q", p, h.ID)
		}
		dup := make([]byte, len(content))
		copy(dup, content)
		out = append(out, CapturedFile{Path: p, Content: dup})
	}
	return out, nil
}

// CopyTo writes each InputFile into the handle's in-mem fs, defensive-copying
// content. Unknown handle is a hard error; len==0 is a no-op. Thread-safe.
func (f *Fake) CopyTo(ctx context.Context, h Handle, files []InputFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, ok := f.handles[h.ID]
	if !ok {
		return fmt.Errorf("container/fake: CopyTo: unknown handle %q", h.ID)
	}
	for _, in := range files {
		dup := make([]byte, len(in.Content))
		copy(dup, in.Content)
		fh.files[in.Path] = dup
	}
	return nil
}

// ReadFileAt implements container.Backend.
func (f *Fake) ReadFileAt(ctx context.Context, h Handle, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, ok := f.handles[h.ID]
	if !ok {
		return nil, fmt.Errorf("container/fake: ReadFileAt: unknown handle %q", h.ID)
	}
	content, ok := fh.files[path]
	if !ok {
		return nil, fmt.Errorf("container/fake: ReadFileAt: path %q not present in handle %q", path, h.ID)
	}
	return cloneBytes(content), nil
}

// ReadTreeAt implements container.Backend. Returns the subtree rooted at dir as
// a gzip-tar (built via BuildTreeTar). Entry paths in the archive are relative
// to dir. Returns an error when no files exist under dir (the caller cannot
// distinguish an empty directory from a missing one in the flat in-memory fs,
// so both are treated as absent).
func (f *Fake) ReadTreeAt(ctx context.Context, h Handle, dir string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, ok := f.handles[h.ID]
	if !ok {
		return nil, fmt.Errorf("container/fake: ReadTreeAt: unknown handle %q", h.ID)
	}
	// Normalise: strip any trailing slash so the prefix comparison is consistent.
	prefix := strings.TrimRight(dir, "/") + "/"
	collected := map[string][]byte{}
	for p, content := range fh.files {
		if strings.HasPrefix(p, prefix) {
			rel := p[len(prefix):]
			if rel == "" {
				continue // bare prefix — skip
			}
			collected[rel] = cloneBytes(content)
		}
	}
	if len(collected) == 0 {
		return nil, fmt.Errorf("container/fake: ReadTreeAt: no files found under %q", dir)
	}
	tarGz, err := BuildTreeTar(collected)
	if err != nil {
		return nil, fmt.Errorf("container/fake: ReadTreeAt: %w", err)
	}
	return tarGz, nil
}

// WriteTreeAt implements container.Backend. Extracts a gzip-tar (as produced
// by BuildTreeTar or ReadTreeAt) into the handle's in-memory fs under dir.
// Relative paths from the archive are joined to dir; existing files are
// overwritten. Enforces the standard TreeTarMaxBytes / TreeTarMaxEntries caps
// to guard against zip-bomb payloads.
func (f *Fake) WriteTreeAt(ctx context.Context, h Handle, dir string, tarGz []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	files, err := ExtractTreeTar(tarGz, TreeTarMaxBytes, TreeTarMaxEntries)
	if err != nil {
		return fmt.Errorf("container/fake: WriteTreeAt: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, ok := f.handles[h.ID]
	if !ok {
		return fmt.Errorf("container/fake: WriteTreeAt: unknown handle %q", h.ID)
	}
	base := strings.TrimRight(dir, "/")
	for rel, content := range files {
		fh.files[base+"/"+rel] = cloneBytes(content)
	}
	return nil
}

// WriteFileAt implements container.Backend.
func (f *Fake) WriteFileAt(ctx context.Context, h Handle, path string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, ok := f.handles[h.ID]
	if !ok {
		return fmt.Errorf("container/fake: WriteFileAt: unknown handle %q", h.ID)
	}
	dup := cloneBytes(content)
	fh.files[path] = dup
	f.WriteFileAtCalls = append(f.WriteFileAtCalls, WriteFileAtCall{Path: path, Content: dup})
	return nil
}

// Snapshot serializes the handle's in-mem files to JSON and Puts them to the
// injected Blobs store, returning the CAS ref. Without an injected store
// (WithBlobs not called) returns ErrUnsupported — the Phase 2 default.
//
// The real Docker backend captures a CoW *diff*; the fake serializes the whole
// file map. This exercises the capture→CAS→ref WIRING (slice 7.1's
// snapshot:workspace round-trip), not Docker's diff fidelity, which the Docker
// tests own.
func (f *Fake) Snapshot(_ context.Context, h Handle) (SnapshotRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blobs == nil {
		return "", ErrUnsupported
	}
	fh, ok := f.handles[h.ID]
	if !ok {
		return "", fmt.Errorf("container/fake: Snapshot: unknown handle %q", h.ID)
	}
	raw, err := json.Marshal(fh.files)
	if err != nil {
		return "", fmt.Errorf("container/fake: Snapshot: marshal files: %w", err)
	}
	ref, err := f.blobs.Put(raw)
	if err != nil {
		return "", fmt.Errorf("container/fake: Snapshot: put: %w", err)
	}
	return SnapshotRef(ref), nil
}

// Restore reads the serialized files from the injected Blobs store and creates
// a fresh handle preloaded with them; records the call in RestoreCalls. Without
// an injected store returns ErrUnsupported — the Phase 2 default. The restored
// handle is built exactly as Create builds one (monotonic-ID key into handles),
// so CaptureFiles works on it unchanged.
func (f *Fake) Restore(_ context.Context, ref SnapshotRef, name string) (Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blobs == nil {
		return Handle{}, ErrUnsupported
	}
	// Record before Get (mirroring Exec/Calls): the restore was *requested* with
	// this (name, ref), so a test asserting on RestoreCalls observes it even if
	// the lookup fails. Don't move this past the error checks.
	f.RestoreCalls = append(f.RestoreCalls, RestoreCall{Name: name, Ref: ref})
	raw, err := f.blobs.Get(string(ref))
	if err != nil {
		return Handle{}, fmt.Errorf("container/fake: Restore: get %q: %w", ref, err)
	}
	var files map[string][]byte
	if err := json.Unmarshal(raw, &files); err != nil {
		return Handle{}, fmt.Errorf("container/fake: Restore: unmarshal: %w", err)
	}
	if files == nil {
		files = map[string][]byte{}
	}
	f.nextID++
	id := fmt.Sprintf("fake-%d", f.nextID)
	f.handles[id] = &fakeHandle{files: files}
	return Handle{Name: name, ID: id}, nil
}

func (f *Fake) Destroy(_ context.Context, h Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DestroyCalls = append(f.DestroyCalls, h)
	if _, ok := f.handles[h.ID]; !ok {
		return fmt.Errorf("container/fake: Destroy: unknown handle %q (already destroyed or never Created)", h.ID)
	}
	delete(f.handles, h.ID)
	return nil
}

// ProgramExec queues a result + optional stream chunks for a Cmd.Run. Calling
// Exec with that Cmd.Run returns the queued result and a closed channel
// pre-filled with the chunks. Test helper, NOT on the Backend interface.
//
// Defensive-copies every byte slice (result.AWFOutput, result.Stdout, each
// chunk's Data) so a caller mutating its slices after ProgramExec returns
// cannot corrupt the fake. Matches state.InMemoryBlobs.Put's discipline.
func (f *Fake) ProgramExec(run string, result ExecResult, chunks []IOChunk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execTable == nil {
		f.execTable = map[string]ExecResult{}
	}
	stored := ExecResult{
		ExitCode:  result.ExitCode,
		AWFOutput: cloneBytes(result.AWFOutput),
		Stdout:    cloneBytes(result.Stdout),
	}
	f.execTable[run] = stored
	if len(chunks) > 0 {
		if f.streamTable == nil {
			f.streamTable = map[string][]IOChunk{}
		}
		dup := make([]IOChunk, len(chunks))
		for i, c := range chunks {
			dup[i] = IOChunk{Stream: c.Stream, Data: cloneBytes(c.Data)}
		}
		f.streamTable[run] = dup
	}
}

// ProgramExecWithFiles is ProgramExec plus the files the programmed command
// WRITES into the executing handle's fs when it runs — simulating output_files
// production on the scripted fake (SP1 Task 8a). The conformance harness creates
// handles internally with no seed hook, so a producer step makes its own
// artifact via this affordance (the written files are then captured by the
// dispatcher's post-Exec CaptureFiles). Defensive-copies the map and every byte
// slice. Existing ProgramExec callers are unaffected (this is a new method).
func (f *Fake) ProgramExecWithFiles(run string, result ExecResult, chunks []IOChunk, files map[string][]byte) {
	f.ProgramExec(run, result, chunks)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fileTable == nil {
		f.fileTable = map[string]map[string][]byte{}
	}
	cp := make(map[string][]byte, len(files))
	for p, b := range files {
		cp[p] = cloneBytes(b)
	}
	f.fileTable[run] = cp
}

// ProgramExecAny is the "match any Cmd.Run" variant of ProgramExec, used
// by tests where the Cmd.Run is built by the caller and the test only
// cares about the response (e.g., agent/claude.Launch_test, where the
// assembled command line is the test SUBJECT, not the lookup key).
//
// The fake keeps a single "any" programmed entry; subsequent calls
// overwrite. Use ProgramExec when you want exact-match lookup. The
// fall-through in Exec consults anyExec only AFTER execTable[cmd.Run]
// misses, so ProgramExec entries still win when both are programmed.
//
// Defensive-copies bytes per the same discipline as ProgramExec.
func (f *Fake) ProgramExecAny(result ExecResult, chunks []IOChunk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := ExecResult{
		ExitCode:  result.ExitCode,
		AWFOutput: cloneBytes(result.AWFOutput),
		Stdout:    cloneBytes(result.Stdout),
		Err:       result.Err,
	}
	f.anyExec = &stored
	if len(chunks) > 0 {
		dup := make([]IOChunk, len(chunks))
		for i, c := range chunks {
			dup[i] = IOChunk{Stream: c.Stream, Data: cloneBytes(c.Data)}
		}
		f.anyChunks = dup
	} else {
		f.anyChunks = nil
	}
}

// cloneBytes returns a copy of b, or nil if b is nil. Local helper so the
// defensive-copy intent is named at call sites.
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	dup := make([]byte, len(b))
	copy(dup, b)
	return dup
}

// cloneCmd defensive-copies a Cmd for Calls recording. The Env map is the
// only non-trivial field — the caller may continue mutating their own map
// after Exec returns, so the copy keeps the recording isolated. Matches
// ProgramExec's defensive-copy discipline.
func cloneCmd(c Cmd) Cmd {
	out := Cmd{Run: c.Run}
	if c.Env != nil {
		out.Env = make(map[string]string, len(c.Env))
		for k, v := range c.Env {
			out.Env[k] = v
		}
	}
	return out
}

// WriteFile preloads a file into the handle's in-mem fs. Test helper for
// setting up CaptureFiles scenarios; not on the Backend interface (Phase 4
// Docker would docker cp instead). Defensive-copies content on the way in.
func (f *Fake) WriteFile(h Handle, path string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, ok := f.handles[h.ID]
	if !ok {
		return fmt.Errorf("container/fake: WriteFile: unknown handle %q", h.ID)
	}
	dup := make([]byte, len(content))
	copy(dup, content)
	fh.files[path] = dup
	return nil
}

// FailExecAfterN configures the fake so the first n Exec calls succeed and
// the (n+1)-th fails with an "induced fault" error. FailExecAfterN(0) fails
// the very first call. One-shot: call #(n+1) and beyond succeed normally —
// matches slice 2.6's bucket-3 use (a single crash per test process; the
// process exits and resumes fresh).
//
// The check sits in Exec itself, so calling this method has no retroactive
// effect on already-completed calls — the count is the live counter at the
// next call.
func (f *Fake) FailExecAfterN(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failExecAt = &n
}

// FailCaptureAfterN is the CaptureFiles analogue — same one-shot semantic,
// same guarantees. Crashes BEFORE any blob is written (CaptureFiles runs
// before Blobs.Put), so resume simply re-runs the step. Useful in slice 2.6
// bucket-2 (replay) to simulate a crash mid-execution before state is
// committed. Bucket-3 (atomic commit) is the job of FailAppendAfterN on
// state.InMemoryLog, which crashes between Blobs.Put and Log.Append(node.completed).
func (f *Fake) FailCaptureAfterN(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCapAt = &n
}

// ClearFault resets BOTH fault hooks (Exec + CaptureFiles). The conformance
// harness uses this between bucket runs that share a fake instance — though
// the standard pattern is to factory() a fresh fake per run/resume (matching
// the "infra rebuilt from recipe" spec §8 semantic), ClearFault is the
// in-place reset used when the same fake survives a crash-resume boundary
// in the conformance harness's atomic-commit bucket.
func (f *Fake) ClearFault() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failExecAt = nil
	f.failCapAt = nil
}

// BlockExec arms the block gate: the NEXT Exec call (and all subsequent calls
// until ReleaseBlockedExec is called) will block inside Exec until released.
// Returns the gate channel — callers may also select on it directly if needed.
// Used by signal conformance tests (Bucket 8) to hold the engine at its first
// step dispatch so the pollControls goroutine can detect a pre-written
// pause/cancel file before any step completes.
//
// Call ReleaseBlockedExec to unblock all waiting Exec calls. If the engine's
// ctx is cancelled while blocked (e.g. by the poller detecting pause/cancel),
// Exec returns ctx.Err — the caller does NOT need to release the gate.
func (f *Fake) BlockExec() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{})
	f.blockExecCh = ch
	return ch
}

// ReleaseBlockedExec unblocks all Exec calls currently waiting on the gate
// channel and disarms the block hook (future Exec calls proceed normally).
// Idempotent — safe to call even if BlockExec was not called or was already
// released.
func (f *Fake) ReleaseBlockedExec() {
	f.mu.Lock()
	ch := f.blockExecCh
	f.blockExecCh = nil
	f.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}
