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
}

// NewInMemoryLog mints a fresh in-memory log. The optional clk must be non-nil; pass
// clock.System{} for "real" time or a clock.Fake for deterministic tests.
func NewInMemoryLog(clk clock.Clock) *InMemoryLog {
	return &InMemoryLog{clk: clk}
}

func (l *InMemoryLog) Append(e Event) error {
	l.seq++
	e.Seq = l.seq
	e.Epoch = l.epoch
	e.TS = l.clk.Now()
	l.events = append(l.events, e)
	return nil
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
// Not safe for concurrent Put/Get from multiple goroutines (matches the single-writer
// assumption the engine relies on, same as InMemoryLog).
type InMemoryBlobs struct {
	store map[string][]byte // key: hex(sha256(content)); value: a copy of the content
}

// NewInMemoryBlobs mints an empty in-memory blob store.
func NewInMemoryBlobs() *InMemoryBlobs {
	return &InMemoryBlobs{store: map[string][]byte{}}
}

func (b *InMemoryBlobs) Put(content []byte) (string, error) {
	hashHex, ref := hashAndRef(content)
	if _, ok := b.store[hashHex]; !ok {
		// Defensive copy — callers might mutate their byte slice after Put.
		dup := make([]byte, len(content))
		copy(dup, content)
		b.store[hashHex] = dup
	}
	return ref, nil
}

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
