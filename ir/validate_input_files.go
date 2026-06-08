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
	order := nodeOrder(wf.Graph)
	outFiles := OutputFilesByStepID(wf)
	// Index maps by static path so a ref into a reduce-declaring map validates against
	// the REDUCER's output_files (SP2 Task 11, validator half — mirrors
	// engine/scope.go ResolveArtifactPath's reduce branch).
	maps := mapsByPath(wf.Graph)

	WalkNodes(wf.Graph, "", func(n Node, nodePath string) {
		if cn, ok := n.(*Compose); ok {
			validateNamedArtifactRef(c, nodePath, "compose.from", cn.From, producers, order, outFiles, maps)
			return
		}
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
			validateNamedArtifactRef(c, nodePath, "input_files["+dst+"]", raw, producers, order, outFiles, maps)
		}
	})
}

func nodeOrder(nodes NodeList) map[string]int {
	out := map[string]int{}
	ord := 0
	WalkNodes(nodes, "", func(_ Node, path string) {
		out[path] = ord
		ord++
	})
	return out
}

func validateNamedArtifactRef(
	c *collector,
	nodePath, label, raw string,
	producers map[string]producer,
	order map[string]int,
	outFiles map[string]OutputFiles,
	maps map[string]*Map,
) {
	if strings.Contains(raw, "{{") || strings.Contains(raw, "}}") {
		c.errf(nodePath, "AWF3007", label+": must be a static step.<id>.files.<name> reference, not a template")
		return
	}
	id, name, ok := template.ParseArtifactRef(raw)
	if !ok {
		c.errf(nodePath, "AWF3007", label+"="+raw+": expected step.<id>.files.<name>")
		return
	}
	p, ok := producers[id]
	if !ok {
		c.errf(nodePath, "AWF3007", label+": step "+id+" is not a declared step")
		return
	}
	if consumerOrd, ok := order[nodePath]; ok && p.ord >= consumerOrd {
		c.errf(nodePath, "AWF3007", label+": producer "+id+" must appear before this consumer")
		return
	}
	// Reduce short-circuit: a producer in the v1 single-map shape, referenced
	// from OUTSIDE the map, whose enclosing map declares a reduce:. The runtime
	// (engine/scope.go ResolveArtifactPath) resolves such a ref against the
	// REDUCER's committed artifact, not the per-item body artifact. So the
	// artifact name must be declared in Reduce.OutputFiles (a run: reducer); a
	// quorum reducer has no artifacts (empty OutputFiles → name not found →
	// AWF3007). The body step's output_files and the gate/map-scope reachability
	// check below do NOT apply — the reducer's output IS reachable from outside.
	if mapPath, _, isMapBody := SingleMapBodyShape(p.path); isMapBody && !pathWithinScope(nodePath, mapPath) {
		if m, ok := maps[mapPath]; ok && m.Reduce != nil {
			if _, ok := m.Reduce.OutputFiles.PathForName(name); !ok {
				c.errf(nodePath, "AWF3007", label+": reduced map (producer "+id+") reducer has no named output_files artifact "+name)
			}
			return
		}
	}
	if _, ok := outFiles[id].PathForName(name); !ok {
		c.errf(nodePath, "AWF3007", label+": step "+id+" has no named output_files artifact "+name)
		return
	}
	if scope, opaque := opaqueScopePrefix(p.path); opaque && !pathWithinScope(nodePath, scope) {
		c.errf(nodePath, "AWF3007", label+": producer "+id+" is inside a gate/map scope not reachable from here")
	}
}
