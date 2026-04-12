package clock

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
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

func TestFakeAdvance(t *testing.T) {
	start := time.Unix(testEpochSeconds, 0).UTC()
	f := &Fake{T: start}
	if got := f.Now(); !got.Equal(start) {
		t.Fatalf("pre-advance Now() = %v", got)
	}
	f.Advance(5 * time.Second)
	if got := f.Now(); got.Sub(start) != 5*time.Second {
		t.Fatalf("after Advance(5s), Now()-start = %v, want 5s", got.Sub(start))
	}
	f.Advance(2 * time.Minute)
	if got := f.Now(); got.Sub(start) != 5*time.Second+2*time.Minute {
		t.Fatalf("after second Advance, Now()-start = %v", got.Sub(start))
	}
}

func TestSystemSleepReturnsAfterDuration(t *testing.T) {
	// Real sleep — production code path. Bounded by a generous deadline so a
	// flaky CI scheduler doesn't trip it; the assertion is "Sleep blocked",
	// not "Sleep blocked for exactly d".
	t.Parallel()
	c := System{}
	start := time.Now()
	if err := c.Sleep(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Errorf("Sleep returned after %v, want at least 5ms", elapsed)
	}
}

func TestSystemSleepRespectsContextCancel(t *testing.T) {
	t.Parallel()
	c := System{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	err := c.Sleep(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep on cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestSystemSleepInSynctestBubble(t *testing.T) {
	// Locks in the methodology that clock.System.Sleep is faked by synctest's
	// time bubble — Phase 4+ timing tests rely on this. Inside the bubble,
	// time.Sleep / time.NewTimer use a fake clock that advances only when all
	// goroutines are durably blocked, so a "1 hour" Sleep returns in zero real
	// time but Now() advances by exactly an hour.
	synctest.Test(t, func(t *testing.T) {
		c := System{}
		start := time.Now()
		if err := c.Sleep(context.Background(), time.Hour); err != nil {
			t.Fatalf("Sleep: %v", err)
		}
		if elapsed := time.Since(start); elapsed != time.Hour {
			t.Errorf("synctest-faked Sleep advanced %v, want exactly 1h", elapsed)
		}
	})
}

func TestFakeSleepAdvancesNowAndIsNonBlocking(t *testing.T) {
	t.Parallel()
	f := &Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	start := time.Now()
	if err := f.Sleep(context.Background(), time.Hour); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("Fake.Sleep blocked real time — should be instant (used %v)", time.Since(start))
	}
	want := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if got := f.Now(); !got.Equal(want) {
		t.Errorf("Fake.Now after Sleep(1h) = %v, want %v", got, want)
	}
}

func TestFakeSleepRespectsContextCancel(t *testing.T) {
	t.Parallel()
	f := &Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := f.Sleep(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Fake.Sleep on cancelled ctx: err = %v, want context.Canceled", err)
	}
	// Advancement on cancel: don't advance (the work didn't happen).
	if got := f.Now(); !got.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Fake.Now after cancelled Sleep = %v, want unchanged", got)
	}
}
