package conformance

import (
	"path/filepath"
	"runtime"
)

// conformanceRepoRoot resolves the repo root via runtime.Caller(0).
// Independent of working directory — works whether the test is invoked
// from conformance/, the repo root, or an IDE's arbitrary CWD.
//
// THIS FILE is at <repoRoot>/conformance/agent_test_helpers.go, so
// the parent of the parent is the repo root.
//
// Caveat: runtime.Caller(0) records the source path at COMPILE time.
// For `go test` (compile + run on the same machine) this is always
// correct. For pre-compiled test binaries shipped to a different
// machine (rare — Go's `go test -c` produces these), the path is the
// compile-time one and resolution breaks. Slice 5.4 doesn't ship
// pre-compiled test binaries.
func conformanceRepoRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(thisFile))
}
