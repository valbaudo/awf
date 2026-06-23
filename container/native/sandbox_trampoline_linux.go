//go:build linux

package native

// __sandbox re-exec trampoline (Linux only).
//
// Landlock self-restricts the calling thread, so the go-landlock sandbox launcher
// (sandbox_linux.go) cannot confine a child from outside it. Instead it re-execs THIS
// binary as:
//
//	<self> __sandbox <policyJSON> -- sh -c <run>
//
// MaybeRunSandboxTrampoline intercepts that re-exec, applies the Landlock policy to
// itself (fail-closed — never degrades to an unrestricted run), then syscall.Exec's
// /bin/sh so the restricted process runs the step command.
//
// Every entry point that can be the re-exec target MUST call this before its normal
// startup: the production `awf` binary calls it from main(), and the container/native
// test binary calls it from TestMain. Without that guard, a trampoline re-exec of the
// caller re-runs the caller's normal path — and for the test binary that means re-running
// the whole suite, which (because the landlock integ tests re-exec again) fork-bombs the
// host. That is exactly what made bwrap-less CI runners OOM.
import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
)

// MaybeRunSandboxTrampoline inspects os.Args; if this process was re-exec'd as
// "<self> __sandbox <policyJSON> -- sh -c <run>" it applies the Landlock policy and
// replaces the process with /bin/sh running the step command (it never returns).
// Otherwise it returns immediately and the caller continues normally (the common case).
func MaybeRunSandboxTrampoline() {
	// argv layout: [self, "__sandbox", policyJSON, "--", "sh", "-c", run]
	if len(os.Args) < 2 || os.Args[1] != "__sandbox" {
		return
	}
	// We are the trampoline. Everything from here is exit-or-exec; no return.
	if len(os.Args) < 7 {
		fmt.Fprintf(os.Stderr, "__sandbox: malformed argv (expected 7+ args, got %d): %v\n", len(os.Args), os.Args)
		os.Exit(2)
	}
	policyJSON := os.Args[2]
	if os.Args[3] != "--" {
		fmt.Fprintf(os.Stderr, "__sandbox: argv[3] = %q, want \"--\"\n", os.Args[3])
		os.Exit(2)
	}
	if os.Args[4] != "sh" || os.Args[5] != "-c" {
		fmt.Fprintf(os.Stderr, "__sandbox: argv[4:7] must be [sh -c <run>], got %v\n", os.Args[4:])
		os.Exit(2)
	}
	run := os.Args[6]

	var policy SandboxPolicy
	if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
		fmt.Fprintf(os.Stderr, "__sandbox: decode policy JSON: %v\n", err)
		os.Exit(2)
	}
	// Apply Landlock FS restrictions. Fail closed — no BestEffort fallback.
	if err := ApplyLandlock(policy.RODirs, policy.RWDirs); err != nil {
		fmt.Fprintf(os.Stderr, "__sandbox: landlock apply: %v\n", err)
		os.Exit(2)
	}
	// Replace this process with /bin/sh executing the step command. The inherited
	// environment was set by exec.go when it spawned the trampoline process.
	if err := syscall.Exec("/bin/sh", []string{"sh", "-c", run}, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "__sandbox: syscall.Exec /bin/sh: %v\n", err)
		os.Exit(2)
	}
	// Unreachable after a successful Exec.
}
