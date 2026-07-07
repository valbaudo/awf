//go:build unix

package native_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

// TestNative_Exec_ReapsGrandchildOnTimeout is the regression test for the native
// timeout deadlock (customer-reported). A step's real workload runs as a
// GRANDCHILD of the awf process: native.Exec launches `sh -c <run>` (and, in
// production, a sandbox trampoline), so the agent/CLI itself is a child of sh.
//
// Before the fix, ctx-cancel used Go's default exec.CommandContext behaviour:
// SIGKILL to the DIRECT child (sh) only. The grandchild survived, holding the
// stdout/stderr pipe write-ends open, so Exec's reader goroutines never saw EOF,
// wg.Wait() blocked forever, c.Wait() was never called, and NO ExecResult was
// ever emitted — the whole run deadlocked. (Docker was fixed by #18/killExecTree;
// native had no equivalent.)
//
// The fix puts the child in its own process group and SIGKILLs the GROUP (-pgid)
// on cancel, plus a WaitDelay backstop — reaping the grandchild and closing the
// pipes. This test asserts (a) the ExecResult is emitted promptly after the ctx
// deadline (no deadlock) and (b) the grandchild is actually reaped (no orphan
// leak). Pre-fix, (a) fails: the select hits the 8s timeout.
func TestNative_Exec_ReapsGrandchildOnTimeout(t *testing.T) {
	b, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := b.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(context.Background(), h) })

	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	// Best-effort: never leak the grandchild sleep if this test fails (pre-fix).
	t.Cleanup(func() {
		if pid, ok := readPIDFile(pidfile); ok {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	// `sh -c` (native's default, no sandbox) is the direct child; the backgrounded
	// sleep is a grandchild of awf that inherits — and holds open — the pipes.
	// Job control is off in a non-interactive `sh -c`, so the sleep stays in sh's
	// process group (which is what the group-kill fix reaps).
	run := "sleep 30 & echo $! > '" + pidfile + "'; wait"

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	chunks, result, err := b.Exec(ctx, h, container.Cmd{Run: run})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	// Drain chunks so the reader goroutines never block on a full buffer.
	go func() {
		for range chunks {
		}
	}()

	select {
	case r := <-result:
		if r.Err == nil {
			t.Errorf("ExecResult.Err = nil; want the ctx deadline error (a timeout must surface)")
		}
		gpid, ok := readPIDFile(pidfile)
		if !ok {
			t.Fatalf("grandchild pidfile %q was never written", pidfile)
		}
		if !waitForProcessGone(gpid, 3*time.Second) {
			t.Errorf("grandchild pid %d still alive after cancel — process tree not reaped (orphan leak)", gpid)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("DEADLOCK: no ExecResult 8s after a 500ms ctx deadline — the grandchild held the pipes open and the reader goroutines never returned (the bug this fixes)")
	}
}

// readPIDFile reads a PID written by the shell, retrying briefly for the write.
func readPIDFile(pidfile string) (int, bool) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if b, err := os.ReadFile(pidfile); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid > 0 {
				return pid, true
			}
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForProcessGone polls signal-0 (existence probe) until the pid is gone
// (ESRCH) or the deadline elapses. Reap is async (the orphaned grandchild is
// reparented to init, which reaps the zombie), so a short poll avoids flakiness.
func waitForProcessGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
