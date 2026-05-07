package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// runLockName is the sidecar liveness lock inside a run directory. It is NOT
// part of the durable state (the append-only log + content-addressed blobs): a
// missing or stale run.lock can never corrupt resume or the artifacts, so the
// "interpreter is the only writer to state" invariant is untouched — the lock is
// pure runtime liveness metadata.
//
// Mechanism (research 2026-05-02): an exclusive BSD flock(2) held by awf run /
// awf resume for the run's lifetime. The kernel releases it on ANY termination
// (clean exit, crash, SIGKILL) with no exit-time action, so a separate prober
// (awf ls) reading the lock distinguishes a live run (lock held) from a crashed
// one (lock free). Go opens files O_CLOEXEC by default, so child processes (the
// agent CLI, docker) do not inherit the fd and cannot keep the lock alive after
// the runner dies. Single-host, no-NFS precondition (over NFS flock degrades to
// fcntl byte-range locks).
const runLockName = "run.lock"

// ErrRunLockHeld means another live process already holds the run lock — the run
// is currently being driven elsewhere. awf run / awf resume refuse on this.
var ErrRunLockHeld = errors.New("run is already active (lock held by a live process)")

// runLock is a held exclusive advisory lock. The holder keeps the fd open for
// the run's lifetime; Release (via defer) drops it on clean exit and the kernel
// drops it on any abnormal exit.
type runLock struct{ f *os.File }

// acquireRunLock opens (creating if absent) <runDir>/run.lock and takes a
// non-blocking exclusive flock. Returns ErrRunLockHeld if a live holder exists.
func acquireRunLock(runDir string) (*runLock, error) {
	path := filepath.Join(runDir, runLockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open run lock %q: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRunLockHeld
		}
		return nil, fmt.Errorf("lock run lock %q: %w", path, err)
	}
	return &runLock{f: f}, nil
}

// Release drops the advisory lock and closes the fd. Best-effort: the kernel
// also releases on process exit, so a Release error is not actionable.
func (l *runLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil // make a double Release a true no-op (the nil guard above)
}

// runLockHeld reports whether a live process holds <runDir>/run.lock. It opens
// the lock file and attempts a non-blocking SHARED flock: EWOULDBLOCK ⇒ a live
// EXCLUSIVE holder (the run process) ⇒ running; success ⇒ no holder (the shared
// lock is immediately released). A missing lock file (legacy run, or one that
// predates this feature) reports not-held. awf ls uses this to split the
// "started, no terminal event" bucket into running (held) vs crashed (not held).
//
// LOCK_SH (NOT LOCK_EX) is load-bearing: a shared probe conflicts with the
// writer's LOCK_EX (→ correctly "held") but NOT with other shared probers, so
// two concurrent `awf ls` invocations never false-positive each other as a live
// run. (An exclusive probe would: the second prober would see the first prober's
// lock and wrongly report "running".) Verified against flock(2) semantics:
// exclusive locks conflict with both shared and exclusive locks held by OTHER
// open file descriptions; shared locks coexist.
func runLockHeld(runDir string) (bool, error) {
	path := filepath.Join(runDir, runLockName)
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
