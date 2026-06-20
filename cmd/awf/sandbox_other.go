//go:build !linux

package main

// maybeSandboxTrampoline is a no-op on non-Linux platforms.
// The __sandbox subcommand only exists on Linux (Landlock is Linux-only).
func maybeSandboxTrampoline() {}
