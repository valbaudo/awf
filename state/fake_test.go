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
