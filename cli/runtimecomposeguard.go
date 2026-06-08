package cli

import (
	"fmt"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// checkRuntimeComposeCapability rejects, at run/resume start, a workflow that
// promotes generated compose bytes on a backend that cannot manage compose
// projects. Native runs on the host and has no compose project lifecycle, so
// accepting such a workflow would silently skip AWF ownership of the generated
// infra.
func checkRuntimeComposeCapability(wf *ir.Workflow, backendName string, backend container.Backend) error {
	path, ok := ir.FirstRuntimeComposePath(wf)
	if !ok {
		return nil
	}
	if backend.Capabilities().RuntimeCompose {
		return nil
	}
	return fmt.Errorf("workflow uses runtime `compose:` at %s, but backend %q cannot promote generated compose projects; use --backend docker or a backend that advertises runtime compose support", path, backendName)
}
