// Package runlock is the sidecar run-liveness lock: an exclusive BSD flock(2) held
// by `awf run` / `awf resume` for a run's lifetime, plus a non-blocking shared probe
// (Held) that distinguishes a live run (lock held) from a crashed one (lock free).
//
// It is NOT durable state — a missing or stale run.lock can never corrupt resume or
// the content-addressed artifacts — so the "interpreter is the only writer to state"
// invariant is untouched. The kernel releases the lock on ANY termination (clean exit,
// crash, SIGKILL), which is what makes the probe meaningful. Go opens files O_CLOEXEC,
// so child processes (the agent CLI, docker) never inherit the fd and cannot keep the
// lock alive after the runner dies. Single-host / no-NFS precondition (over NFS flock
// degrades to fcntl byte-range locks).
//
// This package is the single owner of the lock-file convention; cli (the runner) and
// ui (a read-only liveness prober) both depend on it.
package runlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockName is the sidecar lock file inside a run directory.
const lockName = "run.lock"

// ErrHeld means another live process already holds the run lock — the run is currently
// being driven elsewhere. `awf run` / `awf resume` refuse on this.
var ErrHeld = errors.New("run is already active (lock held by a live process)")

// Lock is a held exclusive advisory lock. The holder keeps the fd open for the run's
// lifetime; Release drops it on clean exit and the kernel drops it on any abnormal exit.
type Lock struct{ f *os.File }

// Acquire opens (creating if absent) <runDir>/run.lock and takes a non-blocking
// exclusive flock. Returns ErrHeld if a live holder exists.
func Acquire(runDir string) (*Lock, error) {
	path := filepath.Join(runDir, lockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open run lock %q: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrHeld
		}
		return nil, fmt.Errorf("lock run lock %q: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the advisory lock and closes the fd. Best-effort: the kernel also
// releases on process exit, so a Release error is not actionable. A double Release is a
// no-op.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// Held reports whether a live process holds <runDir>/run.lock. It attempts a
// non-blocking SHARED flock: EWOULDBLOCK ⇒ a live EXCLUSIVE holder (the run process) ⇒
// running; success ⇒ no holder (the shared lock is immediately released). A missing lock
// file reports not-held. Callers use this to split the "started, no terminal event"
// bucket into running (held) vs crashed (not held).
//
// LOCK_SH (NOT LOCK_EX) is load-bearing: a shared probe conflicts with the writer's
// LOCK_EX (→ correctly "held") but NOT with other shared probers, so two concurrent
// probers never false-positive each other as a live run.
func Held(runDir string) (bool, error) {
	path := filepath.Join(runDir, lockName)
	f, err := os.Open(path) // read-only probe; flock works regardless of access mode
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("open run lock %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("probe run lock %q: %w", path, err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}
