package container

import (
	"context"
	"fmt"
)

// Fake is the in-memory Backend used by Phase 2 engine tests and the
// conformance suite (slice 2.6). Deterministic: monotonic-counter handle
// IDs, no time.Now, no OS-level process spawning, no goroutines. Single-
// writer in Phase 2 (matches state.InMemoryLog precedent); Phase 3
// (parallel) will add per-method synchronization when it lands.
//
// Phase 4 Docker impl will live alongside this in container/docker.go;
// backendtest.RunBasicContract runs against both unchanged.
type Fake struct {
	handles map[string]*fakeHandle
	nextID  int

	// Scripted Exec table — keyed on Cmd.Run.
	execTable   map[string]ExecResult
	streamTable map[string][]IOChunk

	// Fault hooks — set via FailExecAfterN / FailCaptureAfterN (Task 3).
	// nil = no fault. The check in Exec/CaptureFiles is wired now so Task 3
	// is a single setter addition rather than threading code through here.
	execCalls    int
	captureCalls int
	failExecAt   *int
	failCapAt    *int

	// Calls is the defensive-copied history of every Cmd this fake's Exec
	// received. Slice 2.4 (and later) tests inspect this to verify the
	// dispatcher's env-injection contract (AWF_IDEMPOTENCY_KEY per AWF §10).
	// The fake's doc-comment anticipated this need at slice-2.2 time but
	// shipped no mechanism; this is the recording slot.
	Calls []Cmd
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

func (*Fake) Capabilities() Caps {
	// Phase 2 fake never snapshots — Snapshot/Restore return ErrUnsupported.
	return Caps{Snapshot: SnapshotNone}
}

func (f *Fake) Create(_ context.Context, spec ContainerSpec) (Handle, error) {
	f.nextID++
	id := fmt.Sprintf("fake-%d", f.nextID)
	f.handles[id] = &fakeHandle{files: map[string][]byte{}}
	return Handle{Name: spec.Name, ID: id}, nil
}

// Exec returns the ExecResult programmed for cmd.Run (via ProgramExec); the
// channel is buffered with every programmed IOChunk and closed before return.
// Unprogrammed cmd.Run is a hard error — silent zero-value would mask
// dispatcher bugs in slice 2.4. Unknown handle is a hard error. Cmd.Env is
// accepted but not used (slice 2.4 verifies dispatcher env injection by
// inspecting what it passes to Exec, not by what the fake does with it).
func (f *Fake) Exec(ctx context.Context, h Handle, cmd Cmd) (ExecResult, <-chan IOChunk, error) {
	if err := ctx.Err(); err != nil {
		return ExecResult{}, nil, err
	}
	if _, ok := f.handles[h.ID]; !ok {
		return ExecResult{}, nil, fmt.Errorf("container/fake: Exec: unknown handle %q", h.ID)
	}
	// Record the call BEFORE the fault check / programmed lookup — the dispatcher
	// invoked us with this Cmd; the recording is what the env-injection test
	// asserts on regardless of whether the lookup succeeds.
	f.Calls = append(f.Calls, cloneCmd(cmd))
	if f.failExecAt != nil && f.execCalls == *f.failExecAt {
		n := f.execCalls
		f.execCalls++
		return ExecResult{}, nil, fmt.Errorf("container/fake: induced Exec fault at call #%d", n)
	}
	f.execCalls++

	result, ok := f.execTable[cmd.Run]
	if !ok {
		return ExecResult{}, nil, fmt.Errorf("container/fake: Exec: no programmed result for cmd.Run=%q (call ProgramExec first)", cmd.Run)
	}
	chunks := f.streamTable[cmd.Run] // may be nil
	ch := make(chan IOChunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return result, ch, nil
}

// CaptureFiles reads each path from the handle's in-mem fs and returns the
// content in input order. Missing-path errors the whole call; unknown handle
// is a hard error. Defensive-copies content (matches state.InMemoryBlobs.Get
// discipline — callers mutating their slice must not corrupt our store).
func (f *Fake) CaptureFiles(ctx context.Context, h Handle, paths []string) ([]CapturedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

func (*Fake) Snapshot(_ context.Context, _ Handle) (SnapshotRef, error) {
	return "", ErrUnsupported
}

func (*Fake) Restore(_ context.Context, _ SnapshotRef) (Handle, error) {
	return Handle{}, ErrUnsupported
}

func (f *Fake) Destroy(_ context.Context, h Handle) error {
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
func (f *Fake) FailExecAfterN(n int) { f.failExecAt = &n }

// FailCaptureAfterN is the CaptureFiles analogue — same one-shot semantic,
// same guarantees. Crashes BEFORE any blob is written (CaptureFiles runs
// before Blobs.Put), so resume simply re-runs the step. Useful in slice 2.6
// bucket-2 (replay) to simulate a crash mid-execution before state is
// committed. Bucket-3 (atomic commit) is the job of FailAppendAfterN on
// state.InMemoryLog, which crashes between Blobs.Put and Log.Append(node.completed).
func (f *Fake) FailCaptureAfterN(n int) { f.failCapAt = &n }
