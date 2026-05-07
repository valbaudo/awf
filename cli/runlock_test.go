package cli

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestRunLockAcquireProbeRelease(t *testing.T) {
	runDir := t.TempDir()

	// No lock file yet ⇒ not held.
	held, err := runLockHeld(runDir)
	if err != nil {
		t.Fatalf("runLockHeld (absent): %v", err)
	}
	if held {
		t.Fatal("runLockHeld = true with no lock file; want false")
	}

	// Acquire ⇒ a separate open file description (the prober) sees it held.
	lock, err := acquireRunLock(runDir)
	if err != nil {
		t.Fatalf("acquireRunLock: %v", err)
	}
	held, err = runLockHeld(runDir)
	if err != nil {
		t.Fatalf("runLockHeld (held): %v", err)
	}
	if !held {
		t.Fatal("runLockHeld = false while lock is held; want true (running)")
	}

	// A second acquire while held ⇒ ErrRunLockHeld.
	if _, err := acquireRunLock(runDir); !errors.Is(err, ErrRunLockHeld) {
		t.Fatalf("second acquireRunLock err = %v, want ErrRunLockHeld", err)
	}

	// Release ⇒ no live holder (crashed-equivalent: lock free).
	lock.Release()
	held, err = runLockHeld(runDir)
	if err != nil {
		t.Fatalf("runLockHeld (released): %v", err)
	}
	if held {
		t.Fatal("runLockHeld = true after Release; want false (crashed/finished)")
	}
}

func TestRunLockHeldMissingDir(t *testing.T) {
	// A run dir that never created a lock file (legacy run) ⇒ not held, no error.
	held, err := runLockHeld(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("runLockHeld (missing): %v", err)
	}
	if held {
		t.Fatal("runLockHeld = true for missing lock file; want false")
	}
}

func TestRunLockHeldConcurrentProbersDoNotFalsePositive(t *testing.T) {
	// Regression for the LOCK_SH fix: with NO live holder, two simultaneous
	// probers must BOTH report not-held. An exclusive (LOCK_EX) probe would
	// fail here — one prober would grab the lock and the other would see it and
	// wrongly report "running". Shared probes coexist, so both see "not held".
	runDir := t.TempDir()
	// Force the lock file to exist (a crashed run leaves it behind, unlocked).
	lock, err := acquireRunLock(runDir)
	if err != nil {
		t.Fatalf("acquireRunLock: %v", err)
	}
	lock.Release() // simulate the holder having crashed/exited: lock now free.

	const probers = 8
	results := make([]bool, probers)
	errs := make([]error, probers)
	var wg sync.WaitGroup
	for i := 0; i < probers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = runLockHeld(runDir)
		}(i)
	}
	wg.Wait()
	for i := 0; i < probers; i++ {
		if errs[i] != nil {
			t.Fatalf("prober %d: %v", i, errs[i])
		}
		if results[i] {
			t.Errorf("prober %d reported held=true for a free lock (concurrent-prober false positive)", i)
		}
	}
}
