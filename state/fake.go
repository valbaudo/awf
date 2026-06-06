package state

import (
	"fmt"
	"io/fs"
	"sync"

	"github.com/valbaudo/awf/clock"
)

// InMemoryLog is the Log fake: events live in a slice, Sync/Close are no-ops. Used by
// Phase 2's engine tests (the fake backend's conformance suite). Single-writer; not
// safe for concurrent Append from multiple goroutines (matches FileLog's contract).
type InMemoryLog struct {
	clk    clock.Clock
	events []Event
	seq    uint64
	epoch  uint32

	// Fault hooks for slice 2.4 / 2.6 commit-atomicity tests. nil = no fault.
	// One-shot: the (failAppendAt+1)-th Append fails; calls before and after
	// succeed normally. Lives only on the in-mem fake — FileLog stays clean.
	appendCalls  int
	failAppendAt *int
}

// NewInMemoryLog mints a fresh in-memory log. The optional clk must be non-nil; pass
// clock.System{} for "real" time or a clock.Fake for deterministic tests.
func NewInMemoryLog(clk clock.Clock) *InMemoryLog {
	return &InMemoryLog{clk: clk}
}

func (l *InMemoryLog) Append(e Event) error {
	if l.failAppendAt != nil && l.appendCalls == *l.failAppendAt {
		n := l.appendCalls
		l.appendCalls++
		return fmt.Errorf("state/fake: induced Append fault at call #%d", n)
	}
	l.appendCalls++
	l.seq++
	e.Seq = l.seq
	e.Epoch = l.epoch
	e.TS = l.clk.Now()
	l.events = append(l.events, e)
	return nil
}

// FailAppendAfterN configures the fake so the first n Appends succeed and the
// (n+1)-th fails with an "induced fault" error. FailAppendAfterN(0) fails the
// very first call. One-shot: call #(n+1) and beyond succeed normally.
//
// Mirrors container.Fake.FailExecAfterN's semantics; lets slice 2.4's commit
// test crash *between* Blobs.Put and Log.Append for a chosen attempt.
func (l *InMemoryLog) FailAppendAfterN(n int) { l.failAppendAt = &n }

// Reopen simulates a FileLog.OpenLog of an existing file: bumps the internal
// epoch counter to (last-event.Epoch + 1) so subsequent Appends carry the new
// epoch. On an empty log (no events) Reopen is a no-op — matches FileLog's
// "fresh file → epoch=0" semantic.
//
// Conformance harness's crash-then-resume choreography calls this between the
// simulated crash (programmed via Fail*AfterN) and the resume's first Append
// (run.resumed). Mirrors what production FileLog.OpenLog does for free on an
// existing file; the fake doesn't get that for free because it doesn't open
// files.
//
// Returns error for signature symmetry with FileLog.OpenLog (which can fail on
// I/O); the in-mem impl can't actually fail, but callers shouldn't rely on
// that — Phase 4 Docker conformance may swap in a file-backed log where the
// signature matters.
func (l *InMemoryLog) Reopen() error {
	if len(l.events) == 0 {
		return nil
	}
	last := l.events[len(l.events)-1]
	l.epoch = last.Epoch + 1
	return nil
}

// ClearFault resets the FailAppendAfterN hook to "no fault programmed." Idempotent
// on already-cleared. Conformance harness calls this before the resume so a
// programmed crash doesn't replay on the resume's own Appends.
func (l *InMemoryLog) ClearFault() {
	l.failAppendAt = nil
}

func (*InMemoryLog) Sync() error { return nil }

func (l *InMemoryLog) Fold() ([]Event, error) {
	// Return a copy so callers can't mutate our backing array.
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out, nil
}

func (*InMemoryLog) Close() error { return nil }

// InMemoryBlobs is the Blobs fake: a map from hex-hash to content. Matches FSBlobs'
// error contract (ErrBadRef for malformed, wrapped fs.ErrNotExist for missing,
// ErrCorruption never fires in-memory because content can't be tampered with).
// Safe for concurrent Put/Get — a mutex serializes store access so parallel
// branch goroutines (e.g. T10 fan-out) can commit concurrently without racing.
type InMemoryBlobs struct {
	mu    sync.Mutex
	store map[string][]byte // key: hex(sha256(content)); value: a copy of the content

	// Fault hooks for slice 2.4 / 2.6 commit-atomicity tests. nil = no fault.
	// One-shot: the (failPutAt+1)-th Put fails; calls before and after succeed
	// normally. Lives only on the in-mem fake — FSBlobs stays clean.
	putCalls  int
	failPutAt *int
}

// NewInMemoryBlobs mints an empty in-memory blob store.
func NewInMemoryBlobs() *InMemoryBlobs {
	return &InMemoryBlobs{store: map[string][]byte{}}
}

func (b *InMemoryBlobs) Put(content []byte) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failPutAt != nil && b.putCalls == *b.failPutAt {
		n := b.putCalls
		b.putCalls++
		return "", fmt.Errorf("state/fake: induced Put fault at call #%d", n)
	}
	b.putCalls++
	hashHex, ref := hashAndRef(content)
	if _, ok := b.store[hashHex]; !ok {
		// Defensive copy — callers might mutate their byte slice after Put.
		dup := make([]byte, len(content))
		copy(dup, content)
		b.store[hashHex] = dup
	}
	return ref, nil
}

// FailPutAfterN — see InMemoryLog.FailAppendAfterN.
func (b *InMemoryBlobs) FailPutAfterN(n int) { b.failPutAt = &n }

// ClearFault resets the FailPutAfterN hook. See InMemoryLog.ClearFault.
func (b *InMemoryBlobs) ClearFault() {
	b.failPutAt = nil
}

func (b *InMemoryBlobs) Get(ref string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hashHex, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	content, ok := b.store[hashHex]
	if !ok {
		return nil, fmt.Errorf("state: blob %s: %w", ref, fs.ErrNotExist)
	}
	// Defensive copy on read too.
	out := make([]byte, len(content))
	copy(out, content)
	return out, nil
}
