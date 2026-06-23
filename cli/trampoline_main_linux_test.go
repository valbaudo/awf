//go:build linux && integ

package cli_test

import (
	"os"
	"testing"

	"github.com/valbaudo/awf/container/native"
)

// TestMain intercepts the go-landlock __sandbox re-exec for the cli test binary.
//
// The native-backend integ tests (native_resume_integ_test.go,
// run_backend_integ_test.go) drive a REAL `awf run --backend native` in-process via
// cli.Runner.Run (no fake backend injected). With WithSandbox(true) and no bwrap, the
// native backend confines each step by re-execing os.Executable() — here the cli.test
// binary — as "<self> __sandbox <policy> -- sh -c <run>". The production awf binary
// handles that in main() via native.MaybeRunSandboxTrampoline; this test binary must do
// the same, or the re-exec'd cli.test re-runs the cli suite instead of the step command
// (so e.g. step_a never writes its marker and never commits — observed as a hang on
// landlock-capable, bwrap-less CI runners). The non-integ cli tests inject a fake backend
// and never reach this path, which is why this guard is integ-only.
//
// For a normal `go test` invocation os.Args[1] is a test flag, so
// MaybeRunSandboxTrampoline returns immediately and the suite runs as usual.
func TestMain(m *testing.M) {
	native.MaybeRunSandboxTrampoline()
	os.Exit(m.Run())
}
