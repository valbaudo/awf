//go:build unix

package native

import (
	"os/exec"
	"syscall"
)

// hardenProcessCleanup makes ctx-cancel (timeout) reap the whole process TREE,
// not just the direct child.
//
// Native runs the workload under `sh -c <run>` — and, when sandboxed, under a
// bwrap/landlock/sandbox-exec trampoline — so the real agent/CLI process is a
// GRANDCHILD. Go's default exec.CommandContext cancel sends SIGKILL to the direct
// child only; a surviving grandchild keeps the step's stdout/stderr pipe
// write-ends open, so Exec's reader goroutines never see EOF, wg.Wait() never
// returns, c.Wait() is never called, and no ExecResult is ever emitted — the run
// deadlocks. The docker backend fixed the same class with killExecTree (#18);
// native had no equivalent, so native runs stayed exposed.
//
// Fix: Setpgid places the child in its own process group (a non-interactive
// `sh -c` runs with job control OFF, so its backgrounded descendants stay in that
// group), and Cancel SIGKILLs the whole group (-pgid) instead of just the direct
// child — reaping the grandchild and closing the pipes so the readers unblock.
// The caller additionally sets Cmd.WaitDelay (cross-platform) as a backstop for a
// straggler that double-forks out of the group.
func hardenProcessCleanup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		// Cancel only runs after Start, so c.Process is non-nil in practice; guard
		// defensively. The negative PID targets the process GROUP (its pgid equals
		// the group leader's pid, established by Setpgid above).
		if c.Process == nil {
			return nil
		}
		if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
}
