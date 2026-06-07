package cli

import "github.com/valbaudo/awf/runlock"

// The run-liveness lock now lives in package runlock (shared by the runner here and the
// read-only prober in package ui). These thin aliases keep the cli call sites and tests
// unchanged while the convention has a single owner.

// ErrRunLockHeld is returned by acquireRunLock when a live process holds the lock.
var ErrRunLockHeld = runlock.ErrHeld

func acquireRunLock(runDir string) (*runlock.Lock, error) { return runlock.Acquire(runDir) }

func runLockHeld(runDir string) (bool, error) { return runlock.Held(runDir) }
