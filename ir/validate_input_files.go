package ir

import (
	"path"
	"sort"
	"strings"

	"github.com/valbaudo/awf/template"
)

// validateInputFilesModule checks every step's input_files: each value must be either
// a static step.<id>.files.<name> reference (AWF3007) naming a prior reachable
// named output_files artifact, or asset.<id> naming a declared workflow asset.
// Map bodies remain opaque except for reduce: promotion. Gate bodies are special:
// an input_files artifact ref from after a gate may resolve to the accepted
// attempt's artifact at runtime. Scalar step refs remain gate-scoped. Each
// destination path must also be absolute and clean (a format contract for the
// new field).
func validateInputFilesModule(ld *LoadedDefinition, mod validationModule, c *collector) {
	if mod.Workflow == nil {
		return
	}
	wf := mod.Workflow
	producers := map[string]producer{}
	indexModuleProducers(ld, mod.ModuleID, wf.Graph, producers)
	order := nodeOrder(wf.Graph)
	outFiles := outputFilesByStepIDForModule(ld, mod.ModuleID, wf)
	// Index maps by static path so a ref into a reduce-declaring map validates against
	// the REDUCER's output_files (SP2 Task 11, validator half — mirrors
	// engine/scope.go ResolveArtifactPath's reduce branch).
	maps := mapsByPath(wf.Graph)

	WalkNodes(wf.Graph, "", func(n Node, nodePath string) {
		if cn, ok := n.(*Compose); ok {
			validateNamedArtifactRef(c, nodePath, "compose.from", cn.From, producers, order, outFiles, maps)
			return
		}
		// A react: node stages input_files via its OFFERED tool impls. A tool is
		// defined once in wf.Tools but may be offered by several react nodes at
		// different graph positions, so each tool impl's input_files refs are
		// checked relative to THIS react node as the consumer (the producer-order
		// question is per-react-node — the fully-correct wiring). The error is
		// reported at the react node path (where the tool is actually consumed),
		// not at tools.<name> (a step.<id> producer-order verdict has no meaning
		// at a position-less tool definition). P3 review fix.
		if rn, ok := n.(*React); ok {
			validateReactToolInputFiles(c, nodePath, rn, wf, producers, order, outFiles, maps)
			return
		}
		var inputs map[string]string
		// A containerless agent step (uses: awf/llm, no container:) keys input_files
		// by a logical LABEL, not an in-container path: there is no filesystem to
		// stage into, so the destination is a name the adapter attaches to a
		// provider content part. Code steps always have a container; only an
		// AgentStep with an empty Container is containerless.
		containerless := false
		switch s := n.(type) {
		case *CodeStep:
			inputs = s.InputFiles
		case *AgentStep:
			inputs = s.InputFiles
			containerless = s.Container == ""
		default:
			return
		}
		validDsts := make([]string, 0, len(inputs))
		for dst, raw := range inputs {
			if containerless {
				// Label, not a path: must match stepIDPattern (same name charset as
				// workflow input_files names — validate_output_files.go).
				if !stepIDPattern.MatchString(dst) {
					c.errf(nodePath, "AWF3007", "containerless input_files label "+dst+" must match "+stepIDPattern.String())
					continue
				}
			} else if !path.IsAbs(dst) || dst != path.Clean(dst) {
				// dst is a container path: must be absolute + clean (no ".." segment).
				// A format contract for the new field — NOT the security boundary
				// (moby's go-archive contains traversal); use the `path` package
				// (always "/"-separated for container paths), not path/filepath.
				c.errf(nodePath, "AWF3007", "input_files destination "+dst+" must be an absolute, clean path (no '..' segment)")
				continue
			}
			validDsts = append(validDsts, dst)
			validateInputFileRef(c, nodePath, nodePath, "input_files["+dst+"]", raw, wf.InputFiles, wf.Assets, producers, order, outFiles, maps)
		}
		// A containerless step can't have overlapping container paths (labels are
		// flat names; key-uniqueness is already guaranteed by the YAML map). Per-file
		// format/provider compatibility is only knowable at run time when the bytes
		// and resolved provider are in hand — warn that static validation can't.
		if containerless {
			if len(inputs) > 0 {
				c.warnf(nodePath, "AWF2003", catalog["AWF2003"])
			}
			return
		}
		validateInputFileDestinationOverlap(c, nodePath, validDsts)
	})
}

// validateReactToolInputFiles applies the SAME static input_files checks a
// CodeStep/AgentStep gets (AWF3007: well-formed ref, named producer exists,
// producer-order, asset/input declared, absolute+clean dst, destination overlap)
// to every tool impl OFFERED by this react node. Refs are validated with the
// react node as the consumer so a step.<id>.files.<name> producer-order verdict
// is taken relative to THIS react node's graph position (a tool offered by two
// react nodes is checked against each independently). The tool impl's container
// requirement is already enforced by validateTools (AWF1056). P3 review fix.
func validateReactToolInputFiles(
	c *collector,
	reactPath string,
	rn *React,
	wf *Workflow,
	producers map[string]producer,
	order map[string]int,
	outFiles map[string]OutputFiles,
	maps map[string]*Map,
) {
	for _, toolName := range rn.Tools {
		tool, ok := wf.Tools[toolName]
		if !ok {
			continue // unknown tool name → AWF1053 (validateTools); nothing to stage
		}
		inputs := tool.Impl.InputFiles
		if len(inputs) == 0 {
			continue
		}
		validDsts := make([]string, 0, len(inputs))
		for dst, raw := range inputs {
			label := "tool " + toolName + " input_files[" + dst + "]"
			if !path.IsAbs(dst) || dst != path.Clean(dst) {
				c.errf(reactPath, "AWF3007", label+" destination must be an absolute, clean path (no '..' segment)")
				continue
			}
			validDsts = append(validDsts, dst)
			validateInputFileRef(c, reactPath, reactPath, label, raw, wf.InputFiles, wf.Assets, producers, order, outFiles, maps)
		}
		validateInputFileDestinationOverlap(c, reactPath, validDsts)
	}
}

func outputFilesByStepIDForModule(ld *LoadedDefinition, moduleID string, wf *Workflow) map[string]OutputFiles {
	out := OutputFilesByStepID(wf)
	WalkNodes(wf.Graph, "", func(n Node, _ string) {
		call, ok := n.(*CallStep)
		if !ok {
			return
		}
		child, ok := callTargetModule(ld, moduleID, call.Call)
		if !ok || child == nil || child.Workflow == nil || len(child.Workflow.ArtifactExports) == 0 {
			return
		}
		names := make([]string, 0, len(child.Workflow.ArtifactExports))
		for name := range child.Workflow.ArtifactExports {
			names = append(names, name)
		}
		sort.Strings(names)
		ofs := make(OutputFiles, 0, len(names))
		for _, name := range names {
			ofs = append(ofs, OutputFile{Name: name, Path: child.Workflow.ArtifactExports[name]})
		}
		out[call.ID] = ofs
	})
	return out
}

func validateInputFileDestinationOverlap(c *collector, nodePath string, dsts []string) {
	sort.Strings(dsts)
	for i := 1; i < len(dsts); i++ {
		prev := dsts[i-1]
		cur := dsts[i]
		if prev == cur || inputFileDestinationAncestor(prev, cur) {
			c.errf(nodePath, "AWF3007", "input_files destinations "+prev+" and "+cur+" overlap")
		}
	}
}

func inputFileDestinationAncestor(parent, child string) bool {
	if parent == "/" {
		return child != "/"
	}
	return strings.HasPrefix(child, parent+"/")
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

func validateInputFileRef(
	c *collector,
	diagnosticPath, consumerPath, label, raw string,
	workflowInputFiles WorkflowInputFiles,
	assets map[string]string,
	producers map[string]producer,
	order map[string]int,
	outFiles map[string]OutputFiles,
	maps map[string]*Map,
) {
	if strings.Contains(raw, "{{") || strings.Contains(raw, "}}") {
		c.errf(diagnosticPath, "AWF3007", label+": must be a static step.<id>.files.<name> or asset.<id> reference, not a template")
		return
	}
	if name, ok := template.ParseWorkflowInputFileRef(raw); ok {
		if _, declared := workflowInputFiles[name]; !declared {
			c.errf(diagnosticPath, "AWF3007", label+": workflow input file "+name+" is not declared")
		}
		return
	}
	if id, ok := template.ParseAssetRef(raw); ok {
		if _, declared := assets[id]; !declared {
			c.errf(diagnosticPath, "AWF3007", label+": asset "+id+" is not a declared asset")
		}
		return
	}
	id, name, ok := template.ParseArtifactRef(raw)
	if !ok {
		c.errf(diagnosticPath, "AWF3007", label+"="+raw+": expected step.<id>.files.<name> or asset.<id>")
		return
	}
	validateParsedNamedArtifactRef(c, diagnosticPath, consumerPath, label, id, name, producers, order, outFiles, maps)
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
	validateParsedNamedArtifactRef(c, nodePath, nodePath, label, id, name, producers, order, outFiles, maps)
}

func validateParsedNamedArtifactRef(
	c *collector,
	diagnosticPath, consumerPath, label, id, name string,
	producers map[string]producer,
	order map[string]int,
	outFiles map[string]OutputFiles,
	maps map[string]*Map,
) {
	p, ok := producers[id]
	if !ok {
		c.errf(diagnosticPath, "AWF3007", label+": step "+id+" is not a declared step")
		return
	}
	if p.kind == "map_reduce" && pathWithinScope(consumerPath, p.path) {
		c.errf(diagnosticPath, "AWF3007", label+": reduced map product "+id+" may only be referenced outside its producing map")
		return
	}
	if consumerOrd, ok := order[consumerPath]; ok && p.ord >= consumerOrd {
		c.errf(diagnosticPath, "AWF3007", label+": producer "+id+" must appear before this consumer")
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
	if mapPath, _, isMapBody := SingleMapBodyShape(p.path); isMapBody && !pathWithinScope(consumerPath, mapPath) {
		if m, ok := maps[mapPath]; ok && m.Reduce != nil {
			if _, ok := m.Reduce.OutputFiles.PathForName(name); !ok {
				c.errf(diagnosticPath, "AWF3007", label+": reduced map (producer "+id+") reducer has no named output_files artifact "+name)
			}
			return
		}
	}
	if _, ok := outFiles[id].PathForName(name); !ok {
		c.errf(diagnosticPath, "AWF3007", label+": step "+id+" has no named output_files artifact "+name)
		return
	}
	if scope, opaque := opaqueScopePrefix(p.path); opaque && !pathWithinScope(consumerPath, scope) {
		if isGateScope(scope) {
			return
		}
		c.errf(diagnosticPath, "AWF3007", label+": producer "+id+" is inside a gate/map scope not reachable from here")
	}
}

func isGateScope(scope string) bool {
	segs := strings.Split(scope, ".")
	return len(segs) > 0 && strings.HasPrefix(segs[len(segs)-1], "gate[")
}
