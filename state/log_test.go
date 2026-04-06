package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
)

// fixedClock returns the same time on every Now() call. Slice 1.5's Log uses an injected
// Clock so tests can lock TS values; reusing clock.Fake gives us that without a new helper.
func fixedClock(t *testing.T) clock.Clock {
	t.Helper()
	return &clock.Fake{T: time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)}
}

func TestLogAppendFoldRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")

	lg, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()

	for i, typ := range []string{"run.started", "node.started", "node.completed"} {
		if err := lg.Append(Event{Path: "graph[0]", Type: typ, Data: json.RawMessage(`{"i":` + itoa(i) + `}`)}); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}
	if err := lg.Sync(); err != nil {
		t.Fatal(err)
	}
	events, err := lg.Fold()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("Fold returned %d events, want 3", len(events))
	}
	for i, e := range events {
		if want := uint64(i + 1); e.Seq != want {
			t.Errorf("events[%d].Seq = %d, want %d (monotonic from 1)", i, e.Seq, want)
		}
		if e.Epoch != 0 {
			t.Errorf("events[%d].Epoch = %d, want 0 (first open of a fresh log)", i, e.Epoch)
		}
		if e.TS.IsZero() {
			t.Errorf("events[%d].TS is zero (Log must fill from injected Clock)", i)
		}
	}
}

func TestLogReopenIncrementsEpochAndContinuesSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")

	lg, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := lg.Append(Event{Path: "/", Type: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen — epoch must advance, seq must continue.
	lg2, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg2.Close() }()

	if err := lg2.Append(Event{Path: "/", Type: "y"}); err != nil {
		t.Fatal(err)
	}
	if err := lg2.Sync(); err != nil {
		t.Fatal(err)
	}
	events, err := lg2.Fold()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("Fold returned %d events, want 4", len(events))
	}
	last := events[3]
	if last.Seq != 4 {
		t.Errorf("last.Seq = %d, want 4 (monotonic across reopen)", last.Seq)
	}
	if last.Epoch != 1 {
		t.Errorf("last.Epoch = %d, want 1 (incremented on reopen)", last.Epoch)
	}
	for _, e := range events[:3] {
		if e.Epoch != 0 {
			t.Errorf("pre-reopen event has Epoch=%d, want 0 (replayed unchanged)", e.Epoch)
		}
	}
}

func TestLogTornHeaderTruncates(t *testing.T) {
	// Write 2 valid events; then append 3 garbage bytes (less than an 8-byte header) to
	// simulate a crash mid-header-write. Reopen must silently truncate the garbage and Fold
	// must return only the 2 valid events.
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	lg, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := lg.Append(Event{Path: "/", Type: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	// Append torn-header bytes outside the Log's API.
	if err := appendBytesTo(path, []byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatal(err)
	}

	lg2, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg2.Close() }()
	events, err := lg2.Fold()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("Fold = %d events, want 2 (torn header truncated)", len(events))
	}
	// The truncation must persist — file size must now equal exactly the 2 valid records.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size()%8 != 0 {
		t.Errorf("post-truncate file size = %d, must be 8-aligned", info.Size())
	}
}

func TestLogTornPayloadTruncates(t *testing.T) {
	// Write 1 valid event; then append an 8-byte header that claims a 100-byte payload but
	// only follow it with 5 garbage bytes — payload short. Reopen must truncate back to the
	// 1 valid record.
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	lg, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append(Event{Path: "/", Type: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	// Manually append a lying header (len=100, garbage CRC) + 5 payload bytes.
	torn := make([]byte, 8+5)
	torn[0] = 100 // little-endian len=100
	// CRC bytes (4..8) left zero; doesn't matter — short read trips first.
	for i := 0; i < 5; i++ {
		torn[8+i] = 0xAA
	}
	if err := appendBytesTo(path, torn); err != nil {
		t.Fatal(err)
	}

	lg2, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg2.Close() }()
	events, err := lg2.Fold()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("Fold = %d events, want 1 (torn payload truncated)", len(events))
	}
}

func TestLogCRCMismatchTruncates(t *testing.T) {
	// Write 2 valid events; flip a byte inside the second event's payload (offset > 16 so
	// we're in the second record's payload region). Reopen must truncate back to the 1st
	// valid record only.
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	lg, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := lg.Append(Event{Path: "/", Type: "ok", Data: json.RawMessage(`{"k":"v"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the SECOND record's payload. Read the file, find the second header,
	// flip a payload byte after it.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// First record: header at 0; payload starts at 8 with length read from raw[0..4]; the
	// second record's header starts at align8(8 + payloadLen).
	payloadLen0 := uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24
	rec0End := 8 + int(payloadLen0)
	if rem := rec0End % 8; rem != 0 {
		rec0End += 8 - rem
	}
	// Flip a payload byte of the second record (header[rec0End:rec0End+8], payload[rec0End+8:]).
	raw[rec0End+8] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	lg2, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg2.Close() }()
	events, err := lg2.Fold()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("Fold = %d events, want 1 (CRC mismatch truncated 2nd record)", len(events))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := int64(rec0End); info.Size() != got {
		t.Errorf("post-truncate file size = %d, want %d (end of 1st record)", info.Size(), got)
	}
}

func TestLogTornPadTruncates(t *testing.T) {
	// Valid header + valid payload + valid CRC, but the trailing pad bytes are short (the
	// writer crashed mid-pad). decodeFrame's partial-pad branch must classify this as torn,
	// and Open must truncate back to the end of the previous valid record.
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	lg, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append(Event{Path: "/", Type: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	// Produce a real frame via the production codec, then strip one trailing pad byte to
	// simulate the partial-pad torn-tail. Using encodeFrame here keeps the test honest:
	// if the codec ever changes (e.g. different alignment), the test still tests the new
	// "torn final byte" boundary.
	full := encodeFrame([]byte("x")) // 16 bytes = 8 header + 1 payload + 7 pad
	torn := full[:len(full)-1]       // strip one pad byte → 15 bytes, not 8-aligned
	if err := appendBytesTo(path, torn); err != nil {
		t.Fatal(err)
	}

	lg2, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg2.Close() }()
	events, err := lg2.Fold()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("Fold = %d events, want 1 (torn pad truncated 2nd record)", len(events))
	}
}

func TestLogOpenRefusesMissingParent(t *testing.T) {
	// OpenLog deliberately does NOT MkdirAll the parent — the engine in Phase 2 owns the
	// run-directory layout, so plumbing MkdirAll here would couple the primitive to a
	// layout it doesn't own. Lock the behavior so a future "helpful" change is visible.
	dir := t.TempDir()
	path := filepath.Join(dir, "missing-subdir", "log")
	_, err := OpenLog(path, fixedClock(t))
	if err == nil {
		t.Fatal("expected error when parent directory does not exist")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want one wrapping fs.ErrNotExist", err)
	}
}

func TestLogOpenOnEmptyFile(t *testing.T) {
	// Fresh log with a parent that exists: open creates an empty file, Fold returns no events.
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	lg, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	events, err := lg.Fold()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("fresh log Fold = %d events, want 0", len(events))
	}
}

func TestLogCloseImpliesSync(t *testing.T) {
	// Append-then-Close without explicit Sync; reopen must see the event.
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	lg, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append(Event{Path: "/", Type: "close-test"}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	lg2, err := OpenLog(path, fixedClock(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg2.Close() }()
	events, err := lg2.Fold()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "close-test" {
		t.Fatalf("Close did not flush: events = %+v", events)
	}
}

func TestLogInterfaceSatisfiedByFileLog(t *testing.T) {
	// Compile-time check that *FileLog satisfies Log; if this stops compiling, the seam
	// shape changed and Phase 2's engine will break.
	var _ Log = (*FileLog)(nil)
}

// appendBytesTo opens the file for append-only writes outside the Log's API. Used to
// simulate the torn-tail / partial-write conditions a crash would produce.
func appendBytesTo(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// itoa is a tiny zero-alloc int→string helper to keep Append calls inline. Negative ints not
// handled because we never produce them in tests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
