package cli

import (
	"fmt"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// checkSnapshotCapability errors if any declared container is snapshot:workspace
// but the selected backend cannot snapshot (Capabilities().Snapshot ==
// container.SnapshotNone — e.g. the native backend, which has no CoW facility).
//
// Slice 7.1 fail-fast guard: without it a snapshot:workspace run on a
// no-snapshot backend proceeds all the way to the dispatcher's Snapshot call,
// which fails mid-run with container.ErrUnsupported. Catching the mismatch at
// the CLI layer (post-validate, post-backend-construct, pre-Create) is the same
// defense-in-depth pattern as the compose+native rejection in cli/run.go.
//
// The error message MUST contain the literal "snapshot: workspace" so operators
// can grep for the offending field.
func checkSnapshotCapability(wf *ir.Workflow, backend container.Backend) error {
	if backend.Capabilities().Snapshot != container.SnapshotNone {
		return nil
	}
	for name, c := range wf.Containers {
		if c.Snapshot == "workspace" {
			return fmt.Errorf("container %q declares snapshot: workspace but the selected backend does not support snapshots (use --backend docker)", name)
		}
	}
	return nil
}
