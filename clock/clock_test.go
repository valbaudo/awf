package clock

import (
	"testing"
	"time"
)

func TestFakeIsDeterministic(t *testing.T) {
	f := &Fake{T: time.Unix(1700000000, 0).UTC(), IDs: []string{"run-a", "run-b"}}
	if got := f.Now(); !got.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Fatalf("Now() = %v, want %v", got, time.Unix(1700000000, 0).UTC())
	}
	if got := f.NewRunID(); got != "run-a" {
		t.Fatalf("first NewRunID() = %q, want run-a", got)
	}
	if got := f.NewRunID(); got != "run-b" {
		t.Fatalf("second NewRunID() = %q, want run-b", got)
	}
}

func TestProdImplsSatisfyInterfaces(t *testing.T) {
	var _ Clock = System{}
	var _ IDGen = CryptoIDGen{}
	var _ Clock = (*Fake)(nil)
	var _ IDGen = (*Fake)(nil)
	if id := (CryptoIDGen{}).NewRunID(); len(id) != 32 {
		t.Fatalf("run id len = %d, want 32 hex chars", len(id))
	}
}
