package state

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
)

// Compile-time checks: the fakes must satisfy the seam interfaces.
var (
	_ Log   = (*InMemoryLog)(nil)
	_ Blobs = (*InMemoryBlobs)(nil)
)

func TestInMemoryLogSmoke(t *testing.T) {
	clk := &clock.Fake{T: time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)}
	lg := NewInMemoryLog(clk)
	for i := 0; i < 3; i++ {
		if err := lg.Append(Event{Path: "/", Type: "smoke"}); err != nil {
			t.Fatal(err)
		}
	}
	// Sync and Close are no-ops but must not error.
	if err := lg.Sync(); err != nil {
		t.Fatal(err)
	}
	events, err := lg.Fold()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("Fold = %d events, want 3", len(events))
	}
	for i, e := range events {
		if e.Seq != uint64(i+1) {
			t.Errorf("events[%d].Seq = %d, want %d", i, e.Seq, i+1)
		}
		if e.Epoch != 0 {
			t.Errorf("events[%d].Epoch = %d, want 0", i, e.Epoch)
		}
		if !e.TS.Equal(clk.T) {
			t.Errorf("events[%d].TS = %v, want %v", i, e.TS, clk.T)
		}
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInMemoryBlobsSmoke(t *testing.T) {
	bs := NewInMemoryBlobs()
	want := []byte("fake-cas")
	ref, err := bs.Put(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref, "awf-d1:sha256:") {
		t.Errorf("ref %q missing expected prefix", ref)
	}
	// Round-trip.
	got, err := bs.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("Get returned %q, want %q", got, want)
	}
	// Dedup: same bytes → same ref.
	ref2, err := bs.Put(want)
	if err != nil {
		t.Fatal(err)
	}
	if ref != ref2 {
		t.Errorf("dedup broke: %q vs %q", ref, ref2)
	}
	// Missing ref → fs.ErrNotExist (matches FSBlobs contract).
	if _, err := bs.Get("awf-d1:sha256:" + strings.Repeat("e", 64)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Get on missing ref: err = %v, want one wrapping fs.ErrNotExist", err)
	}
	// Malformed ref → ErrBadRef (matches FSBlobs contract).
	if _, err := bs.Get("not-a-ref"); !errors.Is(err, ErrBadRef) {
		t.Errorf("Get on malformed ref: err = %v, want ErrBadRef", err)
	}
}

func TestInMemoryBlobsDefensiveCopy(t *testing.T) {
	// The fake makes a defensive copy on Put so a caller mutating the input slice after Put
	// returns does not corrupt the store. This locks that load-bearing behavior — without
	// it, a future refactor that drops the copy would silently break parity with FSBlobs
	// (which returns a fresh slice from io.ReadAll and is immune by construction).
	bs := NewInMemoryBlobs()
	mut := []byte("mutate-me")
	ref, err := bs.Put(mut)
	if err != nil {
		t.Fatal(err)
	}
	mut[0] = 'X' // mutate input AFTER Put returns
	got, err := bs.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "mutate-me" {
		t.Errorf("Put did not defensive-copy: post-Put mutation leaked into store, got %q", got)
	}
}

func TestInMemoryLogFailAppendAfterN(t *testing.T) {
	t.Parallel()
	lg := NewInMemoryLog(clock.System{})
	lg.FailAppendAfterN(1) // call #1 succeeds, call #2 fails
	if err := lg.Append(Event{Type: "first"}); err != nil {
		t.Fatalf("call #1 unexpected err: %v", err)
	}
	err := lg.Append(Event{Type: "second"})
	if err == nil {
		t.Fatal("call #2 expected to fail, got nil")
	}
	if !strings.Contains(err.Error(), "induced") {
		t.Errorf("err = %v, want mention of 'induced'", err)
	}
	// Call #3 succeeds (one-shot fault).
	if err := lg.Append(Event{Type: "third"}); err != nil {
		t.Errorf("call #3 unexpected err: %v", err)
	}
	// Fold returns only the events that were actually appended (1 and 3).
	events, err := lg.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(events) != 2 || events[0].Type != "first" || events[1].Type != "third" {
		t.Errorf("Fold = %+v, want [first, third]", events)
	}
}

func TestInMemoryLogFailAppendAfterZeroFailsFirst(t *testing.T) {
	t.Parallel()
	lg := NewInMemoryLog(clock.System{})
	lg.FailAppendAfterN(0) // call #1 fails immediately
	err := lg.Append(Event{Type: "first"})
	if err == nil {
		t.Fatal("call #1 expected to fail, got nil")
	}
}

func TestInMemoryBlobsFailPutAfterN(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBlobs()
	b.FailPutAfterN(1) // call #1 succeeds, call #2 fails
	ref1, err := b.Put([]byte("first"))
	if err != nil {
		t.Fatalf("call #1 unexpected err: %v", err)
	}
	_, err = b.Put([]byte("second"))
	if err == nil {
		t.Fatal("call #2 expected to fail, got nil")
	}
	// Call #3 succeeds.
	ref3, err := b.Put([]byte("third"))
	if err != nil {
		t.Fatalf("call #3 unexpected err: %v", err)
	}
	// Only successful Puts are retrievable.
	if got, _ := b.Get(ref1); string(got) != "first" {
		t.Errorf("Get(ref1) = %q, want %q", got, "first")
	}
	if got, _ := b.Get(ref3); string(got) != "third" {
		t.Errorf("Get(ref3) = %q, want %q", got, "third")
	}
}

func TestInMemoryLogReopenBumpsEpoch(t *testing.T) {
	t.Parallel()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := NewInMemoryLog(clk)
	if err := log.Append(Event{Type: "test.first"}); err != nil {
		t.Fatalf("Append #1: %v", err)
	}
	events, _ := log.Fold()
	if len(events) != 1 || events[0].Epoch != 0 {
		t.Fatalf("pre-Reopen: events[0].Epoch = %d, want 0", events[0].Epoch)
	}
	if err := log.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if err := log.Append(Event{Type: "test.second"}); err != nil {
		t.Fatalf("Append #2: %v", err)
	}
	events, _ = log.Fold()
	if len(events) != 2 {
		t.Fatalf("post-Reopen: len(events) = %d, want 2", len(events))
	}
	if events[1].Epoch != 1 {
		t.Errorf("events[1].Epoch = %d, want 1 (bumped on Reopen)", events[1].Epoch)
	}
}

func TestInMemoryLogReopenEmptyIsNoop(t *testing.T) {
	t.Parallel()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := NewInMemoryLog(clk)
	if err := log.Reopen(); err != nil {
		t.Fatalf("Reopen on empty: %v", err)
	}
	if err := log.Append(Event{Type: "test.first"}); err != nil {
		t.Fatalf("Append after empty Reopen: %v", err)
	}
	events, _ := log.Fold()
	if events[0].Epoch != 0 {
		t.Errorf("first event after empty Reopen: Epoch = %d, want 0", events[0].Epoch)
	}
}

func TestInMemoryLogClearFaultResetsFailAt(t *testing.T) {
	t.Parallel()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := NewInMemoryLog(clk)
	log.FailAppendAfterN(0) // first append fails
	log.ClearFault()
	if err := log.Append(Event{Type: "after.clear"}); err != nil {
		t.Errorf("Append after ClearFault: %v (want nil)", err)
	}
}

func TestInMemoryBlobsClearFaultResetsFailAt(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBlobs()
	b.FailPutAfterN(0)
	b.ClearFault()
	if _, err := b.Put([]byte("hello")); err != nil {
		t.Errorf("Put after ClearFault: %v (want nil)", err)
	}
}
