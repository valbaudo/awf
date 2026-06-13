package ir

import (
	"fmt"
	"strings"
)

// validateReduce checks the reduce: clause on every Map. (Parallel is deferred;
// it has no reduce: field — SP2 Task 8.) Exactly one of Quorum/Run; quorum needs
// over:; a run: reducer needs a resolvable container:; quorum's over: must name
// a real body output field; min_success and quorum are mutually exclusive.
//
// The validator owns AWF1035 (reduce shape) and AWF5006 (quorum/over aggregation
// scope). The run: reducer's container resolution reuses checkContainerRef, so an
// undeclared container surfaces as AWF1009 — the same code a code step's bad
// container yields. The reducer has no id and is not in the graph, so its
// output_files/output_schema are not cross-walked here; only its shape is checked.
func validateReduce(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	wf := ld.Workflow
	walkReduce(wf.Graph, "", wf, nil, MapImageTargetOwners(wf), c)
}

func walkReduce(list NodeList, parent string, wf *Workflow, scoped map[string]bool, mapImageTargetOwners map[string][]string, c *collector) {
	for i, n := range list {
		switch v := n.(type) {
		case *CodeStep, *AgentStep, *SignalStep, *CallStep, *Skip, *React:
			// No child maps or reduce clauses. (react has no NodeList body.)
		case *If:
			walkReduce(v.Then, ChildPath(parent, "if", i, "then"), wf, scoped, mapImageTargetOwners, c)
			walkReduce(v.Else, ChildPath(parent, "if", i, "else"), wf, scoped, mapImageTargetOwners, c)
		case *Loop:
			walkReduce(v.Body, ChildPath(parent, "loop", i, "body"), wf, scoped, mapImageTargetOwners, c)
		case *Try:
			walkReduce(v.Do, ChildPath(parent, "try", i, "do"), wf, scoped, mapImageTargetOwners, c)
			walkReduce(v.Catch, ChildPath(parent, "try", i, "catch"), wf, scoped, mapImageTargetOwners, c)
			walkReduce(v.Finally, ChildPath(parent, "try", i, "finally"), wf, scoped, mapImageTargetOwners, c)
		case *Parallel:
			path := PathFor(parent, "parallel", "", i)
			walkReduce(v.Children, path, wf, scoped, mapImageTargetOwners, c)
		case *Gate:
			walkReduce(v.Generate, ChildPath(parent, "gate", i, "generate"), wf, scoped, mapImageTargetOwners, c)
			walkReduce(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), wf, scoped, mapImageTargetOwners, c)
		case *Map:
			path := PathFor(parent, "map", "", i)
			validateMapReduce(v, path, wf, scoped, mapImageTargetOwners, c)
			walkReduce(v.Body, ChildPath(parent, "map", i, "body"), wf, scoped, mapImageTargetOwners, c)
		case *Compose:
			nextScoped := scoped
			if v.As != "" {
				nextScoped = cloneScoped(scoped)
				nextScoped[v.As] = true
			}
			walkReduce(v.Body, ChildPath(parent, "compose", i, "body"), wf, nextScoped, mapImageTargetOwners, c)
		default:
			panic(fmt.Sprintf("ir.walkReduce: unexpected node type %T", n))
		}
	}
}

func validateMapReduce(m *Map, nodePath string, wf *Workflow, scoped map[string]bool, mapImageTargetOwners map[string][]string, c *collector) {
	if m.Reduce == nil {
		return
	}
	r := m.Reduce
	rp := nodePath + ".reduce"
	hasQuorum := r.Quorum != nil
	hasRun := strings.TrimSpace(r.Run) != ""
	switch {
	case hasQuorum == hasRun: // both or neither
		c.errf(rp, "AWF1035", "reduce: must declare exactly one of run: or quorum:")
		return
	case hasQuorum:
		if strings.TrimSpace(r.Over) == "" {
			c.errf(rp, "AWF1035", "reduce: quorum requires over: (the per-branch boolean field)")
			return
		}
		if m.MinSuccess != nil {
			c.errf(rp, "AWF5006", "reduce:{quorum} and min_success are mutually exclusive (quorum generalizes min_success)")
		}
		if !bodyDeclaresField(m.Body, r.Over) {
			c.errf(rp, "AWF5006", "reduce: quorum over: "+r.Over+" is not declared in any body step's output_schema")
		}
	case hasRun:
		if strings.TrimSpace(r.Container) == "" {
			c.errf(rp, "AWF1035", "reduce: run: reducer requires container:")
			return
		}
		checkContainerRefInScope(r.Container, rp, wf, scoped, mapImageTargetOwners, c, true /* required */)
	}
}

// bodyDeclaresField reports whether some step in the map body declares `field`
// in its output_schema properties. Conservative: a schema without an explicit
// properties map (e.g. additionalProperties: true) is treated as declaring the
// field (cannot prove absence).
func bodyDeclaresField(body NodeList, field string) bool {
	found := false
	WalkNodes(body, "", func(n Node, _ string) {
		var schema *JSONSchema
		switch s := n.(type) {
		case *CodeStep:
			schema = s.OutputSchema
		case *AgentStep:
			schema = s.OutputSchema
		}
		if schema == nil {
			return
		}
		props, ok := (*schema)["properties"].(map[string]any)
		if !ok {
			found = true // no explicit properties → cannot disprove
			return
		}
		if _, ok := props[field]; ok {
			found = true
		}
	})
	return found
}
