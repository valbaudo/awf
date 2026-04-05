package ir

import (
	"fmt"
	"strings"
)

// maxExpressionBytes caps the size of an ir.Expr or ir.Template field before the validator
// will attempt to parse it. Adversarial workflows could otherwise submit multi-megabyte
// conditions to OOM the parser; the §7 mini-language has no legitimate need for anything
// longer than a few hundred bytes. 64 KiB is generous.
const maxExpressionBytes = 64 * 1024

// validateStructural runs the AWF1xxx pass: workflow-version, step-id uniqueness, container
// shape, image-is-digest, container-ref resolution (missing or unresolved), parallel/map
// distinct-container rule, loop/map/gate field requirements, expression-size limits, and the
// "no template syntax in static-name fields" rule (AWF1019). All diagnostics produced here
// are Error severity (the only Warnings in slice 1.4 are AWF2002 and AWF3002).
func validateStructural(ld *LoadedDefinition, c *collector) {
	wf := ld.Workflow

	// (a) Workflow-level: only version 1 is defined (AWF §2 "Current: 1").
	if wf.Version != 1 {
		c.errf("", "AWF1017", fmt.Sprintf("%s (got %d)", catalog["AWF1017"], wf.Version))
	}

	// (b) Container shape: each must declare exactly one of image / compose; image must be a
	// digest reference; compose-backed containers must declare a service; neither image nor
	// service field may carry template syntax (those are static names).
	for name, ctr := range wf.Containers {
		switch {
		case ctr.Image != "" && ctr.Compose != "":
			c.errf(ContainerPath(name, ""), "AWF1005", catalog["AWF1005"])
		case ctr.Image == "" && ctr.Compose == "":
			c.errf(ContainerPath(name, ""), "AWF1006", catalog["AWF1006"])
		}
		if ctr.Image != "" && !strings.Contains(ctr.Image, "@sha256:") {
			c.errf(ContainerPath(name, "image"), "AWF1007", catalog["AWF1007"])
		}
		if ctr.Image != "" && strings.Contains(ctr.Image, "{{") {
			c.errf(ContainerPath(name, "image"), "AWF1019", catalog["AWF1019"])
		}
		if ctr.Compose != "" && ctr.Service == "" {
			c.errf(ContainerPath(name, ""), "AWF1008", catalog["AWF1008"])
		}
		if ctr.Service != "" && strings.Contains(ctr.Service, "{{") {
			c.errf(ContainerPath(name, "service"), "AWF1019", catalog["AWF1019"])
		}
	}

	// (c) Walk the graph: step-id uniqueness, container-ref resolution (missing OR unresolved),
	// control-node shape, parallel distinct-container rule, expression-size limits, AWF1019.
	seen := map[string]string{} // step id → first path where seen, for the duplicate diag
	walkStructural(wf.Graph, "", wf, c, seen)
}

// walkStructural recurses into nodes, computing each child's path via PathFor. parent is the
// path of the enclosing node (empty at the top level). wf is read-only — needed for container
// ref resolution (the set of declared container names).
//
// requireContainer is true for CodeStep / AgentStep (where AWF §4 requires a container) and
// false for SignalStep (where AWF §4.3 explicitly states "No container needed").
func walkStructural(nodes NodeList, parent string, wf *Workflow, c *collector, seen map[string]string) {
	for i, n := range nodes {
		switch v := n.(type) {
		case *CodeStep:
			path := PathFor(parent, "", v.ID, i)
			checkStepID(v.ID, path, c, seen)
			checkContainerRef(v.Container, path, wf, c, true /* required */)
			checkFieldSize(v.Run, path, c)
		case *AgentStep:
			path := PathFor(parent, "", v.ID, i)
			checkStepID(v.ID, path, c, seen)
			checkContainerRef(v.Container, path, wf, c, true /* required */)
		case *SignalStep:
			path := PathFor(parent, "", v.ID, i)
			checkStepID(v.ID, path, c, seen)
			// SignalStep has no container — by design (AWF §4.3).
		case *If:
			path := PathFor(parent, "if", "", i)
			checkFieldSize(string(v.Cond), path, c)
			walkStructural(v.Then, path+".then", wf, c, seen)
			walkStructural(v.Else, path+".else", wf, c, seen)
		case *Loop:
			path := PathFor(parent, "loop", "", i)
			if v.Until == nil && v.MaxIters == nil {
				c.errf(path, "AWF1011", catalog["AWF1011"])
			}
			if v.Until != nil {
				checkFieldSize(string(*v.Until), path, c)
			}
			walkStructural(v.Body, path+".body", wf, c, seen)
		case *Try:
			path := PathFor(parent, "try", "", i)
			walkStructural(v.Do, path+".do", wf, c, seen)
			walkStructural(v.Catch, path+".catch", wf, c, seen)
			walkStructural(v.Finally, path+".finally", wf, c, seen)
		case *Parallel:
			path := PathFor(parent, "parallel", "", i)
			checkParallelDistinctContainers(v.Children, path, c)
			walkStructural(v.Children, path, wf, c, seen)
		case *Gate:
			path := PathFor(parent, "gate", "", i)
			if len(v.Generate) == 0 {
				c.errf(path, "AWF1013", catalog["AWF1013"])
			}
			if v.Until == "" {
				c.errf(path, "AWF1015", catalog["AWF1015"])
			} else {
				checkFieldSize(string(v.Until), path, c)
			}
			if len(v.Evaluate) > 0 {
				final := v.Evaluate[len(v.Evaluate)-1]
				if !nodeHasOutputSchema(final) {
					c.errf(path, "AWF1014", catalog["AWF1014"])
				}
			}
			walkStructural(v.Generate, path+".generate", wf, c, seen)
			walkStructural(v.Evaluate, path+".evaluate", wf, c, seen)
		case *Skip:
			// skip has no fields that need structural validation.
		case *Map:
			path := PathFor(parent, "map", "", i)
			if string(v.Over) == "" || v.As == "" || v.Container == "" || v.Concurrency == 0 {
				c.errf(path, "AWF1012", catalog["AWF1012"])
			}
			if v.Over != "" {
				checkFieldSize(string(v.Over), path, c)
			}
			// Map.Container is a static container name (AWF §5.7) — must resolve, no `{{ }}`.
			if v.Container != "" {
				if strings.Contains(v.Container, "{{") {
					c.errf(path, "AWF1019", catalog["AWF1019"])
				} else {
					checkContainerRef(v.Container, path, wf, c, true /* required */)
				}
			}
			walkStructural(v.Body, path+".body", wf, c, seen)
		}
	}
}

func checkStepID(id, path string, c *collector, seen map[string]string) {
	if id == "" {
		return // empty step id surfaces as AWFxxxx in a later slice / Schema; not our concern here.
	}
	if prev, dup := seen[id]; dup {
		c.errf(path, "AWF1004", fmt.Sprintf("%s (first seen at %s)", catalog["AWF1004"], prev))
		return
	}
	seen[id] = path
}

// checkContainerRef emits AWF1009 if a container reference is missing (when required) or
// doesn't resolve to a declared container. required is true for CodeStep / AgentStep / Map;
// false for SignalStep (which has no container by design).
//
// Per AWF §3, "lab:db" syntax addresses a sibling service in the same compose project; only
// the bare name (left of the colon) is checked against the declared container set.
func checkContainerRef(name, path string, wf *Workflow, c *collector, required bool) {
	if name == "" {
		if required {
			c.errf(path, "AWF1009", fmt.Sprintf("%s (container reference is empty)", catalog["AWF1009"]))
		}
		return
	}
	// `{{` in a container ref is the AWF1019 case (handled at the call site for clearer code);
	// the resolve check would also fail here, but the message would be the wrong one.
	if strings.Contains(name, "{{") {
		return // caller emits AWF1019.
	}
	bare := name
	if i := strings.Index(name, ":"); i >= 0 {
		bare = name[:i]
	}
	if _, ok := wf.Containers[bare]; !ok {
		c.errf(path, "AWF1009", fmt.Sprintf("%s (container %q)", catalog["AWF1009"], name))
	}
}

func checkParallelDistinctContainers(children NodeList, path string, c *collector) {
	// §5.4: "branches that run steps MUST target distinct containers / compose projects."
	// Walk each branch's FIRST step and collect the container ref; report a single AWF1010
	// per duplicate pair so the diagnostic count doesn't explode for large parallels.
	used := map[string][]int{} // container name → branch indices using it
	for i, child := range children {
		if ctr := firstContainerRef(child); ctr != "" {
			used[ctr] = append(used[ctr], i)
		}
	}
	for ctr, branches := range used {
		if len(branches) > 1 {
			c.errf(path, "AWF1010", fmt.Sprintf("%s: container %q used by branches %v", catalog["AWF1010"], ctr, branches))
		}
	}
}

// firstContainerRef returns the container name referenced by a node's first step descendent,
// or "" if the node is a pure control structure with no step. Used by the parallel-distinct
// check to determine which container each branch is bound to.
func firstContainerRef(n Node) string {
	switch v := n.(type) {
	case *CodeStep:
		return v.Container
	case *AgentStep:
		return v.Container
	case *If:
		if len(v.Then) > 0 {
			return firstContainerRef(v.Then[0])
		}
		return ""
	case *Loop:
		if len(v.Body) > 0 {
			return firstContainerRef(v.Body[0])
		}
		return ""
	case *Try:
		if len(v.Do) > 0 {
			return firstContainerRef(v.Do[0])
		}
		return ""
	case *Gate:
		if len(v.Generate) > 0 {
			return firstContainerRef(v.Generate[0])
		}
		return ""
	case *Map:
		return v.Container
	case *Parallel:
		// Nested parallel: take the first branch's first container.
		if len(v.Children) > 0 {
			return firstContainerRef(v.Children[0])
		}
		return ""
	}
	return ""
}

func nodeHasOutputSchema(n Node) bool {
	switch v := n.(type) {
	case *CodeStep:
		return v.OutputSchema != nil
	case *AgentStep:
		return v.OutputSchema != nil
	case *SignalStep:
		return v.OutputSchema != nil
	}
	return false
}

// checkFieldSize emits AWF1016 if src exceeds maxExpressionBytes. Applied to every Expr and
// Template field the validator parses — adversarial workflows could otherwise OOM the parser
// with a multi-megabyte condition.
func checkFieldSize(src, path string, c *collector) {
	if len(src) > maxExpressionBytes {
		c.errf(path, "AWF1016", fmt.Sprintf("%s (got %d bytes)", catalog["AWF1016"], len(src)))
	}
}
