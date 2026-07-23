package ir

import (
	"fmt"
	"strings"
)

// validateReduce checks the reduce: clause on every Map. (Parallel is deferred;
// it has no reduce: field — SP2 Task 8.) Exactly one of Quorum/Run; quorum needs
// field:; a run: reducer needs a resolvable container:; quorum's field: must name
// a real body output field; min_success and quorum are mutually exclusive.
//
// The validator owns AWF1035 (reduce shape) and AWF5006 (quorum/field aggregation
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
		if strings.TrimSpace(r.Field) == "" {
			c.errf(rp, "AWF1035", "reduce: quorum requires field: (the per-branch boolean field)")
			return
		}
		if m.MinSuccess != nil {
			c.errf(rp, "AWF5006", "reduce:{quorum} and min_success are mutually exclusive (quorum generalizes min_success)")
		}
		// The verdict commits under field:'s own name alongside the fixed
		// {votes, agree, votes_detail} keys in the SAME output map (engine/reduce.go
		// runQuorumReduce) — field: == one of those would collide (map-literal
		// last-write-wins), silently discarding the verdict or the tally. Reject it
		// loudly instead, applying to every quorum reduce (raw map+quorum and any
		// future sugar built on it).
		if QuorumVerdictFields[r.Field] {
			c.errf(rp, "AWF1068", "quorum field: "+r.Field+" is a reserved verdict key (votes, agree, votes_detail); rename the counted field")
		}
		if !bodyDeclaresField(m.Body, r.Field) {
			c.errf(rp, "AWF5006", "reduce: quorum field: "+r.Field+" is not declared in any body step's output_schema")
		}
	case hasRun:
		if strings.TrimSpace(r.Container) == "" {
			c.errf(rp, "AWF1035", "reduce: run: reducer requires container:")
			return
		}
		checkContainerRefInScope(r.Container, rp, wf, scoped, mapImageTargetOwners, c, true /* required */)
	}

	// A reduce collects body producers by static path; Task-1 resolution handles at
	// most ONE enclosing gate. A producer under a loop (.iter-N) or under a second
	// gate (.attempt-N x2) would miss the lookup and be silently dropped, so reject
	// it loudly here.
	WalkNodes(m.Body, "", func(n Node, p string) {
		switch s := n.(type) {
		case *CodeStep:
			checkReduceProducerNesting(p, s.OutputSchema, s.OutputFiles, rp, c)
		case *AgentStep:
			checkReduceProducerNesting(p, s.OutputSchema, s.OutputFiles, rp, c)
		}
	})
}

// checkReduceProducerNesting emits AWF5007 if a body producer that reduce would
// collect (it declares output_schema or output_files) sits under a loop or more
// than one gate. if/then/else add no runtime multiplicity and are allowed; a
// single gate is allowed (Task-1 forwarding).
func checkReduceProducerNesting(path string, schema *JSONSchema, files OutputFiles, rp string, c *collector) {
	if schema == nil && len(files) == 0 {
		return // not collected by reduce
	}
	gates, loops := 0, 0
	for _, seg := range strings.Split(path, ".") {
		switch {
		case strings.HasPrefix(seg, "gate["):
			gates++
		case strings.HasPrefix(seg, "loop["):
			loops++
		}
	}
	if loops > 0 || gates > 1 {
		c.errf(rp, "AWF5007", "producer "+path+" is under "+fmt.Sprintf("%d loop / %d gate", loops, gates)+" ancestors")
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
