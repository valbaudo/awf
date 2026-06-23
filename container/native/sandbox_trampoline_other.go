//go:build !linux

package native

// MaybeRunSandboxTrampoline is a no-op on non-Linux platforms: the __sandbox re-exec
// trampoline only exists on Linux (Landlock is Linux-only; macOS uses sandbox-exec,
// which wraps the command directly without a re-exec).
func MaybeRunSandboxTrampoline() {}
