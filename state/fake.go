package state

import (
	"fmt"
	"io/fs"

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
// Not safe for concurrent Put/Get from multiple goroutines (matches the single-writer
// assumption the engine relies on, same as InMemoryLog).
type InMemoryBlobs struct {
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

func (b *InMemoryBlobs) Get(ref string) ([]byte, error) {
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
