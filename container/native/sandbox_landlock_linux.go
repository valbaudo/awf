//go:build linux

package native

import (
	"fmt"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// ApplyLandlock enforces a Landlock FS policy in the current process.
// Called by the __sandbox trampoline (native.MaybeRunSandboxTrampoline) BEFORE
// syscall.Exec.
//
// Policy:
//   - RODirs: read-only access (no write, rename, or truncate).
//   - RWDirs: full read+write access.
//   - IgnoreIfMissing: silently skips paths that do not exist on the host
//     (newly-provisioned machines may lack optional dirs like /lib64).
//
// Fail-closed contract: we refuse to run a "sandboxed" step on a kernel with NO
// Landlock support (ABI 0) — silently executing on the unrestricted host would
// violate the faithful-delivery guarantee (promised isolation we can't provide =
// loud failure, never silent host execution). But the confinement we need is only
// FS path restriction (RODirs/RWDirs), which EVERY Landlock ABI (v1+) enforces. So
// we apply with BestEffort, which downgrades from the newest ABI to whatever the
// running kernel actually supports and still restricts the filesystem. (Hard-pinning
// a specific newest ABI like V9 would wrongly reject every kernel between v1 and v8,
// even though they can fully enforce our policy — the CI runner is exactly such a
// kernel.) The explicit ABI>=1 guard ensures BestEffort never degrades to a silent
// no-op on a Landlock-less kernel.
//
// Note: RestrictPaths also sets the "no new privileges" flag in the calling process,
// which is appropriate for untrusted step execution.
func ApplyLandlock(roDirs, rwDirs, roFiles []string) error {
	v, err := llsyscall.LandlockGetABIVersion()
	if err != nil {
		return fmt.Errorf("landlock ABI probe failed: %w", err)
	}
	if v < 1 {
		return fmt.Errorf("kernel has no Landlock support (ABI %d); refusing to run a sandboxed step unconfined", v)
	}
	return landlock.V9.BestEffort().RestrictPaths(
		landlock.RODirs(roDirs...).IgnoreIfMissing(),
		landlock.RWDirs(rwDirs...).IgnoreIfMissing(),
		landlock.ROFiles(roFiles...).IgnoreIfMissing(),
	)
}
