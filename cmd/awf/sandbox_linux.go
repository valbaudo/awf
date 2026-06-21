//go:build linux

package main

// __sandbox re-exec trampoline (Linux only).
//
// When the awf binary is re-launched as:
//
//	awf __sandbox <policyJSON> -- sh -c <run>
//
// this file's maybeSandboxTrampoline() intercepts that invocation BEFORE
// cli.Run, decodes the policy JSON, applies Landlock FS restrictions to the
// current process (fail-closed — never silently degrades), then
// syscall.Exec's /bin/sh so the restricted environment executes the step
// command instead of the full AWF CLI.
//
// Fail-closed contract: if the Landlock apply fails (kernel too old, no
// Landlock ABI support, or any other error) the trampoline writes the error
// to stderr and calls os.Exit(2). It NEVER falls through to cli.Run or
// silently executes the step on the unrestricted host.

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"github.com/valbaudo/awf/container/native"
)

// maybeSandboxTrampoline inspects os.Args and, if the binary was re-exec'd
// as "awf __sandbox <policyJSON> -- …", applies the Landlock policy and
// replaces the current process with /bin/sh running the step command.
//
// If os.Args does not start with "__sandbox", this function returns normally
// and main() continues to cli.Run (the common case).
func maybeSandboxTrampoline() {
	// argv layout: [os.Args[0]="awf", "__sandbox", policyJSON, "--", "sh", "-c", run]
	// os.Args[1] is the first user-visible argument.
	if len(os.Args) < 2 || os.Args[1] != "__sandbox" {
		return
	}

	// We are the trampoline. Everything from here is exit-or-exec; no return.
	if len(os.Args) < 7 {
		fmt.Fprintf(os.Stderr, "awf __sandbox: malformed argv (expected 7+ args, got %d): %v\n", len(os.Args), os.Args)
		os.Exit(2)
	}

	policyJSON := os.Args[2]
	// argv[3] must be "--"; argv[4]="sh", argv[5]="-c", argv[6]=run
	if os.Args[3] != "--" {
		fmt.Fprintf(os.Stderr, "awf __sandbox: argv[3] = %q, want \"--\"\n", os.Args[3])
		os.Exit(2)
	}
	if len(os.Args) < 7 || os.Args[4] != "sh" || os.Args[5] != "-c" {
		fmt.Fprintf(os.Stderr, "awf __sandbox: argv[4:7] must be [sh -c <run>], got %v\n", os.Args[4:])
		os.Exit(2)
	}
	run := os.Args[6]

	var policy native.SandboxPolicy
	if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
		fmt.Fprintf(os.Stderr, "awf __sandbox: decode policy JSON: %v\n", err)
		os.Exit(2)
	}

	// Apply Landlock FS restrictions. Fail closed — no BestEffort fallback.
	if err := native.ApplyLandlock(policy.RODirs, policy.RWDirs); err != nil {
		fmt.Fprintf(os.Stderr, "awf __sandbox: landlock.RestrictPaths: %v\n", err)
		os.Exit(2)
	}

	// Replace this process with /bin/sh executing the step command.
	// The env passed to the trampoline is the inherited process environment
	// (set by exec.go when spawning the trampoline process).
	if err := syscall.Exec("/bin/sh", []string{"sh", "-c", run}, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "awf __sandbox: syscall.Exec /bin/sh: %v\n", err)
		os.Exit(2)
	}
	// Unreachable after successful Exec.
}
