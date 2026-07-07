//go:build !unix

package native

import "os/exec"

// hardenProcessCleanup is a no-op on non-unix platforms: process groups and
// syscall.Kill(-pgid) are unix concepts. The caller still sets Cmd.WaitDelay
// (cross-platform), which bounds the I/O wait so a stalled child cannot deadlock
// Exec's reader goroutines forever even without group-kill reaping.
func hardenProcessCleanup(_ *exec.Cmd) {}
