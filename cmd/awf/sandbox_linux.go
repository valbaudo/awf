//go:build linux

package main

// __sandbox re-exec trampoline (Linux only).
//
// When the awf binary is re-launched as `awf __sandbox <policyJSON> -- sh -c <run>`
// by the go-landlock sandbox launcher, maybeSandboxTrampoline() intercepts it before
// cli.Run, applies the Landlock policy, and execs /bin/sh. The implementation lives in
// the container/native package (native.MaybeRunSandboxTrampoline) so the production
// binary and the package's own test binary apply the identical trampoline — see that
// function's doc for why every re-exec target must call it.

import "github.com/valbaudo/awf/container/native"

// maybeSandboxTrampoline delegates to native.MaybeRunSandboxTrampoline: if os.Args
// starts with "__sandbox" it applies Landlock and execs /bin/sh (never returning);
// otherwise it returns and main() continues to cli.Run.
func maybeSandboxTrampoline() { native.MaybeRunSandboxTrampoline() }
