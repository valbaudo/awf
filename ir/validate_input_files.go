package ir

import (
	"path"
	"strings"

	"github.com/valbaudo/awf/template"
)

// validateInputFiles checks every step's input_files: each value must be a
// static step.<id>.files.<name> reference (AWF3007) naming a prior step that
// declared a NAMED output_files artifact <name> and is reachable in scope
// (same gate/map subtree, via opaqueScopePrefix/pathWithinScope) — matching the
// rigor of the existing step.<id>.<field> ref validator. Each destination path
// must also be absolute and clean (a format contract for the new field).
func validateInputFiles(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	wf := ld.Workflow
	producers := map[string]producer{}
	indexProducers(wf.Graph, producers)
	outFiles := OutputFilesByStepID(wf)

	WalkNodes(wf.Graph, "", func(n Node, nodePath string) {
		var inputs map[string]string
		switch s := n.(type) {
		case *CodeStep:
			inputs = s.InputFiles
		case *AgentStep:
			inputs = s.InputFiles
		default:
			return
		}
		for dst, raw := range inputs {
			// dst is a container path: must be absolute + clean (no ".." segment).
			// A format contract for the new field — NOT the security boundary
			// (moby's go-archive contains traversal); use the `path` package
			// (always "/"-separated for container paths), not path/filepath.
			if !path.IsAbs(dst) || dst != path.Clean(dst) {
				c.errf(nodePath, "AWF3007", "input_files destination "+dst+" must be an absolute, clean path (no '..' segment)")
				continue
			}
			if strings.Contains(raw, "{{") || strings.Contains(raw, "}}") {
				c.errf(nodePath, "AWF3007", "input_files["+dst+"]: must be a static step.<id>.files.<name> reference, not a template")
				continue
			}
			id, name, ok := template.ParseArtifactRef(raw)
			if !ok {
				c.errf(nodePath, "AWF3007", "input_files["+dst+"]="+raw+": expected step.<id>.files.<name>")
				continue
			}
			p, ok := producers[id]
			if !ok {
				c.errf(nodePath, "AWF3007", "input_files["+dst+"]: step "+id+" is not a declared step")
				continue
			}
			if _, ok := outFiles[id].PathForName(name); !ok {
				c.errf(nodePath, "AWF3007", "input_files["+dst+"]: step "+id+" has no named output_files artifact "+name)
				continue
			}
			if scope, opaque := opaqueScopePrefix(p.path); opaque && !pathWithinScope(nodePath, scope) {
				c.errf(nodePath, "AWF3007", "input_files["+dst+"]: producer "+id+" is inside a gate/map scope not reachable from here")
			}
		}
	})
}
