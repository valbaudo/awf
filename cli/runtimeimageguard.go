package cli

import (
	"fmt"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// checkRuntimeImageCapability rejects, at run start, a workflow that uses a
// map's runtime-resolved image: on a backend that cannot honor it (P6a). Native
// ignores spec.Image and runs on the host, so a runtime image there would
// silently execute bodies on the host with no isolation and no digest capture —
// fail closed. Mirrors checkSnapshotCapability (cli/snapshotguard.go).
func checkRuntimeImageCapability(wf *ir.Workflow, backend container.Backend) error {
	if len(ir.MapImageTargets(wf)) == 0 {
		return nil // no runtime-image map; nothing to guard
	}
	if backend.Capabilities().RuntimeImage {
		return nil
	}
	return fmt.Errorf("workflow uses a map `image:` (a runtime-resolved per-element image), which the selected backend cannot honor — it would ignore image: and run bodies on the host without isolation or digest capture. No backend can run a map `image:` workflow yet. To run today, replace the map's `image:` with a static, digest-pinned `containers:` entry (image: ...@sha256:...) and target it with `container:` instead")
}
