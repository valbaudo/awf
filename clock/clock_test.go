package clock

import (
	"testing"
	"time"
)

// testEpochSeconds is a deterministic fixed timestamp (2023-11-14T22:13:20Z), pinned so tests
// don't depend on wall-clock or timezone.
const testEpochSeconds = 1700000000

func TestFakeIsDeterministic(t *testing.T) {
	want := time.Unix(testEpochSeconds, 0).UTC()
	f := &Fake{T: want, IDs: []string{"run-a", "run-b"}}
	if got := f.Now(); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
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
	if id := (CryptoIDGen{}).NewRunID(); len(id) != runIDHexLen {
		t.Fatalf("run id len = %d, want %d hex chars", len(id), runIDHexLen)
	}
}
