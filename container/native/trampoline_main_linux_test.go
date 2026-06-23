//go:build linux && integ

package native_test

import (
	"os"
	"testing"

	"github.com/valbaudo/awf/container/native"
)

// TestMain intercepts the __sandbox re-exec the go-landlock sandbox launcher performs.
//
// The landlock integ tests (sandbox_linux_integ_test.go) drive a real sandboxed Exec,
// which re-execs THIS test binary as "<self> __sandbox <policy> -- sh -c <run>". The
// production awf binary handles that in main() via native.MaybeRunSandboxTrampoline; the
// test binary must do the same here. Without this guard the re-exec'd test binary re-runs
// the whole suite instead of applying Landlock and exec'ing /bin/sh — and because the
// landlock tests re-exec again, that recursion fork-bombs the host (observed as an OOM /
// SIGTERM on bwrap-less CI runners, where the trampoline path is taken).
//
// For a normal `go test` invocation os.Args[1] is a test flag, so
// MaybeRunSandboxTrampoline returns immediately and the suite runs as usual.
func TestMain(m *testing.M) {
	native.MaybeRunSandboxTrampoline()
	os.Exit(m.Run())
}
