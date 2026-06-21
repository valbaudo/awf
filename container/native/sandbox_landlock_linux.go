//go:build linux

package native

import (
	"github.com/landlock-lsm/go-landlock/landlock"
)

// ApplyLandlock enforces a Landlock FS policy in the current process.
// Called by the __sandbox trampoline in cmd/awf/main.go BEFORE syscall.Exec.
//
// Policy:
//   - RODirs: read-only access (no write, rename, or truncate).
//   - RWDirs: full read+write access.
//   - IgnoreIfMissing: silently skips paths that do not exist on the host
//     (newly-provisioned machines may lack optional dirs like /lib64).
//
// Fail-closed contract: BestEffort() is deliberately NOT used. If the kernel
// cannot enforce Landlock V9 (e.g. old kernel, no Landlock built in), this
// returns a non-nil error. The trampoline MUST propagate that error to stderr
// and exit non-zero — silently running on the host would violate the
// faithful-delivery guarantee (promised isolation we can't provide = loud
// failure, never silent host execution).
//
// Note: RestrictPaths also sets the "no new privileges" flag in the calling
// process, which is appropriate for untrusted step execution.
// ApplyLandlock is exported for use by the cmd/awf __sandbox trampoline.
func ApplyLandlock(roDirs, rwDirs []string) error {
	return landlock.V9.RestrictPaths(
		landlock.RODirs(roDirs...).IgnoreIfMissing(),
		landlock.RWDirs(rwDirs...).IgnoreIfMissing(),
	)
}
