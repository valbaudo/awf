// Package state provides the durability core — an append-only Log and content-addressed
// Blobs — that the engine (Phase 2) sits on for commit/resume and that obs (Phase 6) reads
// to project OTel spans.
//
// The Log is etcd-style framed (length + CRC32C + payload + 8-byte pad — see event.go); the
// fold scans from offset 0 and silently truncates on the first short read or CRC mismatch,
// which is the torn-tail recovery the design requires. The Blobs store is content-addressed
// (sha256 v1; the ref format carries the algorithm name so BLAKE3 can land additively).
//
// Slice 1.5 ships the primitives; the engine in Phase 2 orchestrates them into the
// content-address-then-pointer-swap commit boundary (CLAUDE.md invariant: artifacts in
// Blobs first, then a `node.completed` event appended + fsync'd).
package state

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/valbaudo/awf/clock"
)

// Log is the durability seam. One concrete impl (FileLog) + one fake (InMemoryLog).
//
//   - Append assigns Seq (monotonic across reopens), Epoch (incremented on each open of an
//     existing log), and TS (from the injected Clock); the caller fills Type, Path,
//     PayloadRef, Data.
//   - Sync fsyncs the file; caller decides per-event criticality (the engine in Phase 2
//     calls Sync on node.completed, run.*, signal.received — the design's durability-critical
//     events — and lets high-frequency io.chunk/agent.event ride the next fsync).
//   - Fold rescans the file and returns the full event sequence. No internal cache (the
//     access pattern is dominated by Append; Fold is called on resume / for debugging).
//   - Close fsyncs and releases the file handle.
type Log interface {
	Append(e Event) error
	Sync() error
	Fold() ([]Event, error)
	Close() error
}

// FileLog is the production impl — one append-only file per run. Live agent
// event sinks may append concurrently with interpreter commits.
type FileLog struct {
	mu    sync.Mutex
	path  string
	file  *os.File
	clk   clock.Clock
	seq   uint64 // last assigned; next is seq+1
	epoch uint32 // current open's epoch
}

// OpenLog opens (or creates) the log file at path. On an existing file, it scans to the
// first short read or CRC mismatch and truncates the file there (torn-tail recovery),
// then initializes the next Seq to (max-seen + 1) and the Epoch to (max-seen + 1).
// On a fresh file, Seq starts at 0 (first Append gets Seq=1) and Epoch is 0.
//
// OpenLog does NOT create intermediate directories — the parent must exist. (The engine
// in Phase 2 owns the run directory layout, so plumbing MkdirAll here would couple the
// primitive to a layout it doesn't own.)
func OpenLog(path string, clk clock.Clock) (*FileLog, error) {
	return openLog(path, clk, os.O_RDWR|os.O_CREATE|os.O_APPEND)
}

// OpenLogExisting opens an existing log for append and performs the same
// scan/torn-tail repair as OpenLog, but never creates the file. Resume uses this
// only after it owns the run lock, ensuring that observers and losing resume
// contenders cannot mutate a log merely by trying to open it.
func OpenLogExisting(path string, clk clock.Clock) (*FileLog, error) {
	return openLog(path, clk, os.O_RDWR|os.O_APPEND)
}

func openLog(path string, clk clock.Clock, flags int) (*FileLog, error) {
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, fmt.Errorf("state: open log %q: %w", path, err)
	}
	lg := &FileLog{path: path, file: f, clk: clk}

	// Fold existing content to find the last valid record. Reading from offset 0 with a
	// separate read is fine even though the handle is O_APPEND (O_APPEND only forces writes
	// to file-end; reads obey the file pointer).
	events, validBytes, err := scanFile(f)
	if err != nil {
		// scanFile only errors on I/O; torn-tail / CRC are silent.
		_ = f.Close()
		return nil, fmt.Errorf("state: scan log %q: %w", path, err)
	}

	// Truncate any torn tail. If the file is fully clean, validBytes == size, Truncate is
	// a no-op; if torn, Truncate cuts back to the last good record.
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("state: stat log %q: %w", path, err)
	}
	if validBytes < info.Size() {
		if err := f.Truncate(validBytes); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("state: truncate torn tail of %q: %w", path, err)
		}
		// Re-seek to end of valid prefix. (O_APPEND repositions on each write, but Truncate
		// does not move the file offset; subsequent writes still go to file-end via O_APPEND.
		// The seek is defensive — it lets any non-append read see the same EOF the writer will.)
		if _, err := f.Seek(validBytes, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("state: seek after truncate %q: %w", path, err)
		}
	}

	if n := len(events); n > 0 {
		last := events[n-1]
		lg.seq = last.Seq
		lg.epoch = last.Epoch + 1
	}
	return lg, nil
}

// OpenLogExclusive opens (or creates) the log file at path WITH O_EXCL — i.e.
// the open atomically fails if the file already exists. This is the race-free
// first-run primitive: `awf run` calls it to mint a new run.id's log, and the
// existing OpenLog (which tolerates and torn-tail-recovers an existing file)
// stays as the resume primitive.
//
// On collision, the returned error wraps fs.ErrExist — callers route with
// errors.Is(err, fs.ErrExist) for the "run id already exists, use awf resume"
// message.
//
// Unlike OpenLog, there's no torn-tail-recovery branch here: a fresh file
// can't have a torn tail. The function is intentionally minimal.
func OpenLogExclusive(path string, clk clock.Clock) (*FileLog, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("state: open log %q exclusive: %w", path, err)
	}
	return &FileLog{path: path, file: f, clk: clk}, nil
}

// Append assigns Seq/Epoch/TS and writes the framed JSON record. Does NOT fsync — caller
// invokes Sync at durability-critical events.
func (lg *FileLog) Append(e Event) error {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.seq++
	e.Seq = lg.seq
	e.Epoch = lg.epoch
	e.TS = lg.clk.Now()

	payload, err := marshalEvent(e)
	if err != nil {
		lg.seq-- // roll back the counter — the event didn't land
		return fmt.Errorf("state: marshal event seq=%d: %w", e.Seq, err)
	}
	frame := encodeFrame(payload)
	if _, err := lg.file.Write(frame); err != nil {
		lg.seq--
		return fmt.Errorf("state: append event seq=%d: %w", e.Seq, err)
	}
	return nil
}

// Sync fsyncs the underlying file.
func (lg *FileLog) Sync() error {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if err := lg.file.Sync(); err != nil {
		return fmt.Errorf("state: sync log %q: %w", lg.path, err)
	}
	return nil
}

// Fold re-reads the file from offset 0 and returns every record decoded as an Event.
// Torn-tail records are silently skipped (Open already truncated them; this is the
// post-truncation read).
func (lg *FileLog) Fold() ([]Event, error) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	// Re-open a read handle so the seek/read interaction with the O_APPEND write handle
	// doesn't race for the file offset. Phase 1 is single-writer; the read handle is
	// short-lived (within this Fold call).
	rf, err := os.Open(lg.path)
	if err != nil {
		return nil, fmt.Errorf("state: open log %q for fold: %w", lg.path, err)
	}
	defer func() { _ = rf.Close() }() // read-only handle; Close error not meaningful here
	events, _, err := scanFile(rf)
	return events, err
}

// FoldFile reads + decodes every committed event from the log at path
// WITHOUT taking a writable file handle. Use this when you need to
// observe a log that another process or goroutine may be actively
// writing to — unlike OpenLog, FoldFile never truncates a torn tail
// (the file is opened read-only). Torn-tail records at the tail are
// silently skipped per scanFile's semantics.
//
// Single-writer discipline is preserved: only OpenLog/OpenLogExclusive
// take a write handle; FoldFile is observer-only.
func FoldFile(path string) ([]Event, error) {
	rf, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("state: open log %q for fold: %w", path, err)
	}
	defer func() { _ = rf.Close() }()
	events, _, err := scanFile(rf)
	return events, err
}

// Close fsyncs and closes. If Sync fails we still attempt Close (releasing the FD is more
// important than the trailing close-error), but the Sync error is what we return.
func (lg *FileLog) Close() error {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if err := lg.file.Sync(); err != nil {
		_ = lg.file.Close()
		return fmt.Errorf("state: close log %q (sync): %w", lg.path, err)
	}
	if err := lg.file.Close(); err != nil {
		return fmt.Errorf("state: close log %q: %w", lg.path, err)
	}
	return nil
}

// scanFile reads every record from f's current offset (callers pass a freshly opened
// handle, so offset is 0). Returns the decoded events, the byte offset of the end of the
// last good record (== file size if fully clean), and any I/O error. Torn-tail / CRC-fail
// records are NOT errors — they're the "uncommitted frontier" the design requires us to
// recover from.
func scanFile(f *os.File) ([]Event, int64, error) {
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	var events []Event
	off := 0
	for off < len(buf) {
		payload, consumed, derr := decodeFrame(buf[off:])
		if derr != nil {
			// Torn tail (errShortFrame or errCRCMismatch): stop here. The byte offset of the
			// last good record's end is `off`; everything from `off` to len(buf) is the torn
			// tail Open will truncate.
			return events, int64(off), nil
		}
		e, uerr := unmarshalEvent(payload)
		if uerr != nil {
			// CRC matched but JSON didn't parse. This is corruption that bypassed the CRC
			// (very unlikely — CRC32 has ~2^-32 false-accept rate per block). Treat like
			// torn-tail: stop and let Open truncate.
			return events, int64(off), nil
		}
		events = append(events, e)
		off += consumed
	}
	return events, int64(off), nil
}
