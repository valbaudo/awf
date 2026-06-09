package ir

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/valbaudo/awf/template"
)

// producer is the per-step record indexed by step id. Used by AWF3001 to look up declared
// output_schema fields and by AWF3002 to detect agent schemas with no inbound ref. The
// schema field is *JSONSchema (matching the struct field type on Step nodes); nil means
// "no output_schema declared." Dereference at use sites.
type producer struct {
	path   string
	ord    int
	kind   string // "code", "agent", "signal", "input", "map_reduce", "map_compact"
	schema *JSONSchema
	reduce *Reduce
}

// validateRefs runs the AWF3001/AWF3002 pass: walks every ir.Template and ir.Expr field,
// extracts references via the template package, and cross-checks them against the
// producers' output_schemas — extended to also cross-check `input.<field>` against the
// workflow's input schema.
//
// The two rules:
//
//   - AWF3001 (Error): any `step.<id>.<field>` reference must resolve to a declared step
//     whose `output_schema` lists `<field>` in `properties` (or `<field>` ∈ {exit_code,
//     stdout}, which are implicit on every code step). Also covers `input.<field>` —
//     resolved against the workflow's top-level `input` schema (treated as a synthetic
//     "input" producer).
//   - AWF3002 (Warning): every agent step that declares an `output_schema` must have at
//     least one `step.<id>.<field>` reference into it — otherwise the schema is dead weight
//     (the §7 iff-referenced contract).
//
// Reference roots understood by the walker: `step.<id>.<field>`, `input.<field>`,
// `evaluate.<field>` (legal only inside gate.generate or gate.until — enforced statically by
// AWF5001 via the evaluateAllowed bool threaded through walkRefs), `run.id` (always OK),
// `<as>.<field>` (only meaningful inside a map; deferred to Phase 2's evaluator scope check).
func validateRefs(ld *LoadedDefinition, c *collector) {
	wf := ld.Workflow

	// Build the producer index: step.id → (kind, output_schema). Used to check AWF3001.
	producers := map[string]producer{}
	indexProducers(wf.Graph, producers)

	// Synthetic "input" producer so the same checkRef machinery can validate input.<field>
	// against wf.Input. Path "input" mirrors what TestSchemaInputSchemaAlsoValidated uses.
	if wf.Input != nil {
		producers["input"] = producer{path: "input", kind: "input", schema: wf.Input}
	}

	// Index every Map by its static IR path so checkRef's AWF5004 branch can read the
	// enclosing map's Reduce: a ref INTO a reduce-declaring map resolves against the
	// REDUCER's output (SP2 Task 11, validator half — mirrors engine/scope.go
	// aggregateMapOutputs short-circuiting to LookupCompleted(mapStatic)).
	maps := mapsByPath(wf.Graph)

	// Track which producers had at least one ref into them (for AWF3002).
	referenced := map[string]bool{}

	// Walk the graph collecting refs from every Template and Expr field. evaluateAllowed=false
	// and no over-sink map at the top level — only the gate frame's generate/until flip
	// evaluateAllowed true, and only a map's over: carries its map path as the array sink.
	walkRefs(wf.Graph, "", c, producers, maps, referenced, false, "")

	// AWF3002: any AgentStep with an output_schema but no inbound ref → warning.
	for id, p := range producers {
		if p.kind == "agent" && p.schema != nil && !referenced[id] {
			c.warnf(p.path, "AWF3002", fmt.Sprintf("%s (step %q)", catalog["AWF3002"], id))
		}
	}
}

// SingleMapBodyShape reports whether staticPath is the v1 aggregation shape:
// exactly one map[ boundary, no gate[ or loop[ anywhere, the map segment followed
// by "body". Returns the map's static path ("...map[N]") and the producer's suffix
// after ".body.". The single owner of this grammar predicate (engine uses it too).
//
// Suffix contract: suffix has NO leading dot (it is the path segments after ".body.",
// joined by "."); callers compose it as ItemPath(mapPath, N) + "." + suffix.
func SingleMapBodyShape(staticPath string) (mapPath, suffix string, ok bool) {
	segs := strings.Split(staticPath, ".")
	mapIdx, mapCount := -1, 0
	for i, seg := range segs {
		switch {
		case strings.HasPrefix(seg, "map["):
			mapCount++
			mapIdx = i
		case strings.HasPrefix(seg, "gate["), strings.HasPrefix(seg, "loop["):
			return "", "", false
		}
	}
	if mapCount != 1 || mapIdx < 0 || mapIdx+1 >= len(segs) || segs[mapIdx+1] != "body" {
		return "", "", false
	}
	return strings.Join(segs[:mapIdx+1], "."), strings.Join(segs[mapIdx+2:], "."), true
}

// mapsByPath indexes every Map by its static IR path in one graph walk. Keyed by
// the SAME path strings SingleMapBodyShape returns (e.g. "map[0]",
// "gate[0].generate.map[1]"), so checkRef can resolve the enclosing *Map from a
// producer's single-map mapPath and read its Reduce clause.
func mapsByPath(nodes NodeList) map[string]*Map {
	out := map[string]*Map{}
	WalkNodes(nodes, "", func(n Node, path string) {
		if m, ok := n.(*Map); ok {
			out[path] = m
		}
	})
	return out
}

func indexProducers(nodes NodeList, producers map[string]producer) {
	ord := 0
	WalkNodes(nodes, "", func(n Node, path string) {
		currentOrd := ord
		ord++
		switch v := n.(type) {
		case *CodeStep:
			producers[v.ID] = producer{path: path, ord: currentOrd, kind: "code", schema: v.OutputSchema}
		case *AgentStep:
			producers[v.ID] = producer{path: path, ord: currentOrd, kind: "agent", schema: v.OutputSchema}
		case *SignalStep:
			producers[v.ID] = producer{path: path, ord: currentOrd, kind: "signal", schema: v.OutputSchema}
		case *Map:
			if v.ID == "" {
				return
			}
			if v.Reduce != nil {
				producers[v.ID] = producer{
					path:   path,
					ord:    currentOrd,
					kind:   "map_reduce",
					schema: v.Reduce.OutputSchema,
					reduce: v.Reduce,
				}
				return
			}
			_, schema, _ := MapCompactProducer(v)
			producers[v.ID] = producer{path: path, ord: currentOrd, kind: "map_compact", schema: schema}
		}
	})
}

// NOTE: not built on ir.WalkNodes — walkRefs threads gate-asymmetric
// evaluateAllowed (true into generate/until, false into evaluate; propagated
// unchanged elsewhere). That is per-child-branch state a uniform pre-order
// visitor cannot carry; this is a fold, not a visit. Keep bespoke.
//
// walkRefs visits every Template and Expr field in the graph and processes its refs.
//
// evaluateAllowed gates emission of AWF5001 in checkRef's `evaluate` case: true means
// `evaluate.<field>` is legal in this subtree, false means it errors. The bool propagates
// unchanged through every non-Gate node, so it represents the innermost gate frame's
// allow/deny — nested gates OVERRIDE (the inner frame's value is what walkRefs passes down).
//
// overSinkMapPath is non-empty ONLY inside a map's over: expression — the one array-native
// sink. It is threaded so checkRef's step case can allow an aggregate (array-typed) ref there
// and emit AWF5004 everywhere else. Every non-over call site passes "" (over: is a single Expr
// with no recursion, so the sink never propagates into a subtree). Named compact map products
// also use the path to reject self-references from their own over: expression.
func walkRefs(nodes NodeList, parent string, c *collector, producers map[string]producer, maps map[string]*Map, referenced map[string]bool, evaluateAllowed bool, overSinkMapPath string) {
	for i, n := range nodes {
		switch v := n.(type) {
		case *CodeStep:
			path := PathFor(parent, "", v.ID, i)
			checkTemplateRefs(v.Run, path+".run", c, producers, maps, referenced, evaluateAllowed, "")
			if v.IdempotencyKey != nil {
				checkTemplateRefs(string(*v.IdempotencyKey), path+".idempotency_key", c, producers, maps, referenced, evaluateAllowed, "")
			}
		case *AgentStep:
			path := PathFor(parent, "", v.ID, i)
			if v.IdempotencyKey != nil {
				checkTemplateRefs(string(*v.IdempotencyKey), path+".idempotency_key", c, producers, maps, referenced, evaluateAllowed, "")
			}
			// Walk top-level string leaves of v.With in sorted key order (stable diagnostics).
			// This mirrors engine.substituteRawConfig which templates every top-level string
			// value — key-agnostic and opacity-preserving: we inspect only strings, never
			// recurse into nested structures, and never interpret any key name.
			if len(v.With) > 0 {
				keys := make([]string, 0, len(v.With))
				for k := range v.With {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					sv, ok := v.With[k].(string)
					if !ok {
						continue
					}
					checkTemplateRefs(sv, path+".with."+k, c, producers, maps, referenced, evaluateAllowed, "")
				}
			}
		case *SignalStep:
			// no Template / Expr fields beyond the schema itself.
		case *If:
			path := PathFor(parent, "if", "", i)
			checkExprRefs(string(v.Cond), path+".cond", c, producers, maps, referenced, evaluateAllowed, "")
			walkRefs(v.Then, ChildPath(parent, "if", i, "then"), c, producers, maps, referenced, evaluateAllowed, "")
			walkRefs(v.Else, ChildPath(parent, "if", i, "else"), c, producers, maps, referenced, evaluateAllowed, "")
		case *Loop:
			path := PathFor(parent, "loop", "", i)
			if v.Until != nil {
				checkExprRefs(string(*v.Until), path+".until", c, producers, maps, referenced, evaluateAllowed, "")
			}
			walkRefs(v.Body, ChildPath(parent, "loop", i, "body"), c, producers, maps, referenced, evaluateAllowed, "")
		case *Try:
			walkRefs(v.Do, ChildPath(parent, "try", i, "do"), c, producers, maps, referenced, evaluateAllowed, "")
			walkRefs(v.Catch, ChildPath(parent, "try", i, "catch"), c, producers, maps, referenced, evaluateAllowed, "")
			walkRefs(v.Finally, ChildPath(parent, "try", i, "finally"), c, producers, maps, referenced, evaluateAllowed, "")
		case *Parallel:
			walkRefs(v.Children, PathFor(parent, "parallel", "", i), c, producers, maps, referenced, evaluateAllowed, "")
		case *Gate:
			path := PathFor(parent, "gate", "", i)
			// gate.until: evaluate.* allowed (single Expr field, no recursion).
			checkExprRefs(string(v.Until), path+".until", c, producers, maps, referenced, true, "")
			// gate.generate: evaluate.* allowed (innermost frame OVERRIDES enclosing).
			walkRefs(v.Generate, ChildPath(parent, "gate", i, "generate"), c, producers, maps, referenced, true, "")
			// gate.evaluate: evaluate.* REJECTED (the evaluator can't reference its own in-flight output).
			walkRefs(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), c, producers, maps, referenced, false, "")
		case *Map:
			path := PathFor(parent, "map", "", i)
			// over: is the one array-native sink — an aggregate ref is legal here (overSink=true).
			checkExprRefs(string(v.Over), path+".over", c, producers, maps, referenced, evaluateAllowed, path)
			// v.Container is a STATIC container name (AWF §5.7); validated by walkStructural
			// (AWF1009/AWF1019). Not a Template — no Slots/ParseRef walk here.
			walkRefs(v.Body, ChildPath(parent, "map", i, "body"), c, producers, maps, referenced, evaluateAllowed, "")
			checkReduceRefs(v.Reduce, path, c, producers, maps, referenced, evaluateAllowed)
		case *Compose:
			path := PathFor(parent, "compose", "", i)
			checkTemplateRefs(string(v.Service), path+".service", c, producers, maps, referenced, evaluateAllowed, "")
			walkRefs(v.Body, ChildPath(parent, "compose", i, "body"), c, producers, maps, referenced, evaluateAllowed, "")
		}
	}
}

func checkReduceRefs(r *Reduce, mapPath string, c *collector, producers map[string]producer, maps map[string]*Map, referenced map[string]bool, evaluateAllowed bool) {
	if r == nil {
		return
	}
	if r.Run != "" {
		checkReduceTemplateRefs(r.Run, mapPath+".reduce.run", c, producers, maps, referenced, evaluateAllowed)
	}
	for _, of := range r.OutputFiles {
		if of.Name == "" {
			checkReduceTemplateRefs(of.Path, mapPath+".reduce.output_files", c, producers, maps, referenced, evaluateAllowed)
			continue
		}
		checkReduceTemplateRefs(of.Path, mapPath+".reduce.output_files."+of.Name, c, producers, maps, referenced, evaluateAllowed)
	}
}

func checkReduceTemplateRefs(src, path string, c *collector, producers map[string]producer, maps map[string]*Map, referenced map[string]bool, evaluateAllowed bool) {
	if src == "" {
		return
	}
	slots, err := template.Slots(src)
	if err != nil {
		c.errf(path, "AWF3001", fmt.Sprintf("malformed template: %s", syntaxMessage(err)))
		return
	}
	for _, sl := range slots {
		inner := strings.TrimSpace(sl.Inner)
		if inner == "" {
			c.errf(path, "AWF3001", "empty {{ }} slot")
			continue
		}
		ref, err := template.ParseRef(inner)
		if err != nil {
			c.errf(path, "AWF3001", fmt.Sprintf("invalid reference %q: %s", inner, syntaxMessage(err)))
			continue
		}
		checkReduceRef(*ref, path, c, producers, maps, referenced, evaluateAllowed)
	}
}

// Reducer template fields are scanned to catch named map-product self-refs while
// preserving the historical body-step aggregate contract inside reducers.
func checkReduceRef(ref template.Ref, path string, c *collector, producers map[string]producer, maps map[string]*Map, referenced map[string]bool, evaluateAllowed bool) {
	if len(ref.Segments) >= 2 && ref.Segments[0].Ident == "step" && !ref.Segments[1].IsIndex {
		id := ref.Segments[1].Ident
		if p, ok := producers[id]; ok {
			if mapPath, _, isAgg := SingleMapBodyShape(p.path); isAgg && pathWithinScope(path, mapPath+".reduce") {
				referenced[id] = true
				if len(ref.Segments) == 2 {
					return
				}
				if ref.Segments[2].IsIndex {
					c.errf(path, "AWF3001", fmt.Sprintf("malformed step reference (need step.<id>.<field>): %s", renderRef(ref)))
					return
				}
				field := ref.Segments[2].Ident
				if field == "exit_code" || field == "stdout" {
					c.errf(path, "AWF5005", fmt.Sprintf("%s: %s", catalog["AWF5005"], renderRef(ref)))
					return
				}
				checkSchemaField(c, path, id, field, p.schema)
				return
			}
		}
	}
	checkRef(ref, path, c, producers, maps, referenced, evaluateAllowed, "")
}

// checkTemplateRefs scans src (an ir.Template field) for `{{ … }}` slots, parses each as a
// ref via the template package, and runs each through checkRef. evaluateAllowed is propagated
// to checkRef so the `evaluate.<field>` scope rule (AWF5001) can fire. overSinkMapPath is
// propagated so the step case can allow an aggregate ref (and emit AWF5004 elsewhere).
func checkTemplateRefs(src, path string, c *collector, producers map[string]producer, maps map[string]*Map, referenced map[string]bool, evaluateAllowed bool, overSinkMapPath string) {
	if src == "" {
		return
	}
	slots, err := template.Slots(src)
	if err != nil {
		c.errf(path, "AWF3001", fmt.Sprintf("malformed template: %s", syntaxMessage(err)))
		return
	}
	for _, sl := range slots {
		inner := strings.TrimSpace(sl.Inner)
		if inner == "" {
			c.errf(path, "AWF3001", "empty {{ }} slot")
			continue
		}
		ref, err := template.ParseRef(inner)
		if err != nil {
			c.errf(path, "AWF3001", fmt.Sprintf("invalid reference %q: %s", inner, syntaxMessage(err)))
			continue
		}
		checkRef(*ref, path, c, producers, maps, referenced, evaluateAllowed, overSinkMapPath)
	}
}

// checkExprRefs unwraps the outer `{{ }}` envelope (if present), parses the inner as an
// Expr via the template package, and runs each Ref in the AST through checkRef. evaluateAllowed
// is propagated to checkRef so the `evaluate.<field>` scope rule (AWF5001) can fire.
// overSinkMapPath is propagated so the step case can allow an aggregate ref (only a map's
// over: passes a non-empty path).
func checkExprRefs(src, path string, c *collector, producers map[string]producer, maps map[string]*Map, referenced map[string]bool, evaluateAllowed bool, overSinkMapPath string) {
	if src == "" {
		return
	}
	inner := template.UnwrapEnvelope(src)
	e, err := template.ParseExpr(inner)
	if err != nil {
		c.errf(path, "AWF3001", fmt.Sprintf("invalid expression: %s", syntaxMessage(err)))
		return
	}
	for _, ref := range template.References(e) {
		checkRef(ref, path, c, producers, maps, referenced, evaluateAllowed, overSinkMapPath)
	}
}

// checkSchemaField emits AWF3001 if the producer declares no output_schema or the
// schema doesn't declare field; returns true iff field is valid (no diagnostic emitted).
// Shared by checkRef's aggregate and non-aggregate step branches.
func checkSchemaField(c *collector, path, id, field string, schema *JSONSchema) bool {
	if schema == nil {
		c.errf(path, "AWF3001", fmt.Sprintf("reference to step %q field %q but no output_schema declared", id, field))
		return false
	}
	props, _ := (*schema)["properties"].(map[string]any)
	if _, ok := props[field]; !ok {
		c.errf(path, "AWF3001", fmt.Sprintf("step %q output_schema does not declare field %q", id, field))
		return false
	}
	return true
}

// QuorumVerdictFields is the fixed typed-output shape a quorum reducer commits.
// It is the SINGLE source of truth for the {passed, votes, agree} verdict
// contract: this validator binds downstream refs against it, and engine's
// runQuorumReduce must produce EXACTLY these keys — pinned by a cross-package
// engine test (TestQuorumReduceOutputMatchesVerdictFields) so the producer
// (engine/reduce.go) and the validator can never silently drift. Exported only
// for that test.
var QuorumVerdictFields = map[string]bool{"passed": true, "votes": true, "agree": true}

// checkReducedMapRef validates a `step.<bodyId>[.<field>]` reference into a map that
// declares a reduce:. The ref resolves against the REDUCER's committed output (the
// runtime's LookupCompleted(mapStatic)), NOT the per-item aggregate — so:
//
//   - 2-seg `step.<bodyId>` → the reducer's whole output (a scalar object); accepted.
//   - 3-seg `step.<bodyId>.<field>`:
//   - run: reducer → <field> must be declared in Reduce.OutputSchema (AWF3001 else).
//   - quorum reducer → <field> ∈ {passed, votes, agree} (AWF3001 else).
//
// exit_code/stdout are not a reducer's typed output (the reducer is not the body
// code step), so they hit the schema/quorum field check and fail with AWF3001 —
// the same as any non-declared field. An index segment is malformed.
//
// Marks the body step referenced (suppresses AWF3002) only when the ref is well-formed
// enough to be a real read of the reduced output.
func checkReducedMapRef(c *collector, path, id string, ref template.Ref, r *Reduce, referenced map[string]bool) {
	referenced[id] = true
	if len(ref.Segments) == 2 { // step.<id> → the reducer's whole output (scalar object)
		return
	}
	if ref.Segments[2].IsIndex {
		c.errf(path, "AWF3001", fmt.Sprintf("malformed step reference (need step.<id>.<field>): %s", renderRef(ref)))
		return
	}
	field := ref.Segments[2].Ident
	if r.IsQuorum() {
		if !QuorumVerdictFields[field] {
			c.errf(path, "AWF3001", fmt.Sprintf("step %q is a quorum-reduced map; the reduced verdict declares only {passed, votes, agree}, not field %q", id, field))
		}
		return
	}
	// run: reducer — validate against the reducer's output_schema.
	checkSchemaField(c, path, id, field, r.OutputSchema)
}

// checkRef classifies a ref by its first segment and applies the appropriate cross-check.
// evaluateAllowed controls whether `evaluate.<field>` is legal in this position — false
// emits AWF5001 (the static counterpart of engine.Scope.resolveEvaluate's runtime check).
// overSinkMapPath is non-empty only inside a map's over: expression — the one array-native
// sink where an aggregate (array-typed) ref is legal; elsewhere an aggregate ref emits
// AWF5004.
func checkRef(ref template.Ref, path string, c *collector, producers map[string]producer, maps map[string]*Map, referenced map[string]bool, evaluateAllowed bool, overSinkMapPath string) {
	if len(ref.Segments) == 0 {
		return
	}
	root := ref.Segments[0].Ident
	switch root {
	case "step":
		if len(ref.Segments) < 2 || ref.Segments[1].IsIndex {
			c.errf(path, "AWF3001", fmt.Sprintf("malformed step reference (need step.<id>.<field>): %s", renderRef(ref)))
			return
		}
		id := ref.Segments[1].Ident
		p, ok := producers[id]
		if !ok {
			c.errf(path, "AWF3001", fmt.Sprintf("reference to undeclared step %q", id))
			return
		}
		if (p.kind == "map_reduce" || p.kind == "map_compact") && pathWithinScope(path, p.path) {
			c.errf(path, "AWF5010", fmt.Sprintf("%s: %s", catalog["AWF5010"], renderRef(ref)))
			return
		}
		if p.kind == "map_reduce" {
			checkReducedMapRef(c, path, id, ref, p.reduce, referenced)
			return
		}
		if p.kind == "map_compact" {
			if overSinkMapPath == "" || overSinkMapPath == p.path {
				c.errf(path, "AWF5004", fmt.Sprintf("%s: %s", catalog["AWF5004"], renderRef(ref)))
				return
			}
			referenced[id] = true
			if len(ref.Segments) == 2 { // step.<map-id> → []object
				return
			}
			if ref.Segments[2].IsIndex {
				c.errf(path, "AWF3001", fmt.Sprintf("malformed step reference (need step.<id>.<field>): %s", renderRef(ref)))
				return
			}
			field := ref.Segments[2].Ident
			if field == "exit_code" || field == "stdout" {
				c.errf(path, "AWF5005", fmt.Sprintf("%s: %s", catalog["AWF5005"], renderRef(ref)))
				return
			}
			checkSchemaField(c, path, id, field, p.schema)
			return
		}
		// Aggregate read: producer inside the v1 single-map shape, ref site OUTSIDE it.
		if mapPath, _, isAgg := SingleMapBodyShape(p.path); isAgg {
			if pathWithinScope(path, mapPath) && !pathWithinScope(path, mapPath+".body") {
				c.errf(path, "AWF5002", fmt.Sprintf("%s: %s", catalog["AWF5002"], renderRef(ref)))
				return
			}
			if !pathWithinScope(path, mapPath) {
				// Reduce short-circuit (SP2 Task 11, validator half): if the enclosing map
				// declared a reduce:, the per-item aggregate is REPLACED by the reducer's
				// single committed output (engine/scope.go aggregateMapOutputs prefers
				// LookupCompleted(mapStatic) when present). So the ref resolves against the
				// REDUCER's output shape — a SCALAR object, not an array — and AWF5004 does
				// NOT apply (it is the wrong diagnostic for a non-aggregate). Validate the
				// field against the reducer's output shape instead, EXACTLY what the runtime
				// resolves: a run: reducer → its output_schema; a quorum reducer → the fixed
				// {passed,votes,agree} verdict shape. Mirrors regardless of overSink because
				// the reduced output is scalar in both positions.
				if m, ok := maps[mapPath]; ok && m.Reduce != nil {
					checkReducedMapRef(c, path, id, ref, m.Reduce, referenced)
					return
				}
				if overSinkMapPath == "" {
					c.errf(path, "AWF5004", fmt.Sprintf("%s: %s", catalog["AWF5004"], renderRef(ref)))
					return
				}
				referenced[id] = true
				if len(ref.Segments) == 2 { // step.<id> → []object
					return
				}
				if ref.Segments[2].IsIndex {
					c.errf(path, "AWF3001", fmt.Sprintf("malformed step reference (need step.<id>.<field>): %s", renderRef(ref)))
					return
				}
				field := ref.Segments[2].Ident
				if field == "exit_code" || field == "stdout" {
					c.errf(path, "AWF5005", fmt.Sprintf("%s: %s", catalog["AWF5005"], renderRef(ref)))
					return
				}
				checkSchemaField(c, path, id, field, p.schema)
				return
			}
		}
		// Non-aggregate: require step.<id>.<field>.
		if len(ref.Segments) < 3 || ref.Segments[2].IsIndex {
			c.errf(path, "AWF3001", fmt.Sprintf("malformed step reference (need step.<id>.<field>): %s", renderRef(ref)))
			return
		}
		field := ref.Segments[2].Ident
		// AWF5003 / AWF5002: gate and map bodies are opaque multiplicity scopes. A step inside
		// one resolves only from within the same attempt/item (structurally: the reference site
		// must be inside the producer's enclosing gate/map subtree). A reference from outside has
		// no defined attempt/item — the static counterpart of engine.Scope.stepRuntimePath's
		// same-attempt/same-item check. loop / try / parallel are transparent (loops via the
		// "most recent iteration" rule) and don't trigger this. The single-map aggregate shape
		// was handled above; a still-opaque map scope here means nested/loop-multiplied maps
		// (aggregation not yet defined → AWF5002), a gate scope means AWF5003.
		if scope, opaque := opaqueScopePrefix(p.path); opaque && !pathWithinScope(path, scope) {
			// Reaching here means the ref is out-of-scope and NOT the v1 single-map
			// aggregate (that branch returned above). So the producer is either inside a
			// gate (read its product via evaluate.<field> → AWF5003) or inside a map with
			// NO gate, i.e. nested/loop-multiplied maps (aggregation deferred → AWF5002).
			if !strings.Contains(p.path, "gate[") {
				c.errf(path, "AWF5002", fmt.Sprintf("%s: %s", catalog["AWF5002"], renderRef(ref)))
				return
			}
			c.errf(path, "AWF5003", fmt.Sprintf("%s: %s", catalog["AWF5003"], renderRef(ref)))
			return
		}
		// exit_code and stdout are implicit on every code step (AWF §4.1).
		// Agent/signal steps produce typed output via output_schema; they have no exit_code/stdout.
		if field == "exit_code" || field == "stdout" {
			if p.kind != "code" {
				c.errf(path, "AWF3001", fmt.Sprintf("step %q is a %s step, not a code step — %s is only defined for code steps (§4.1)", id, p.kind, field))
				return
			}
			referenced[id] = true
			return
		}
		// Field must be declared in producer's output_schema.
		if !checkSchemaField(c, path, id, field, p.schema) {
			return
		}
		referenced[id] = true
	case "input":
		// input.<field> resolves against the workflow's top-level input schema. Treat input
		// as a synthetic producer with the workflow's Input as its schema. The synthetic
		// producer is registered in validateRefs (see the "input" entry).
		if len(ref.Segments) < 2 || ref.Segments[1].IsIndex {
			c.errf(path, "AWF3001", fmt.Sprintf("malformed input reference (need input.<field>): %s", renderRef(ref)))
			return
		}
		field := ref.Segments[1].Ident
		p, ok := producers["input"]
		if !ok || p.schema == nil {
			c.errf(path, "AWF3001", fmt.Sprintf("reference to input.%s but workflow declares no `input:` schema", field))
			return
		}
		props, _ := (*p.schema)["properties"].(map[string]any)
		if _, ok := props[field]; !ok {
			c.errf(path, "AWF3001", fmt.Sprintf("workflow input schema does not declare field %q", field))
		}
	case "run":
		// Per AWF §7, the only defined run ref is run.id.
		if len(ref.Segments) != 2 || ref.Segments[1].IsIndex || ref.Segments[1].Ident != "id" {
			c.errf(path, "AWF3001", fmt.Sprintf("only `run.id` is defined under the `run` root: %s", renderRef(ref)))
		}
	case "evaluate":
		// evaluate.<field> is only legal inside a gate's generate sub-tree or in the gate's
		// until expression — walkRefs flips evaluateAllowed=true when entering those positions.
		// AWF5001 is the static counterpart of engine.Scope.resolveEvaluate's runtime check.
		if !evaluateAllowed {
			c.errf(path, "AWF5001", fmt.Sprintf("%s: %s", catalog["AWF5001"], renderRef(ref)))
		}
	default:
		// Unknown root (e.g. an `<as>` binding from a map) — slice 1.4 doesn't track binding
		// scopes; defer to Phase 2's evaluator scope check.
	}
}

// opaqueScopePrefix returns the static path of the INNERMOST gate or map body
// enclosing staticPath, and ok=false when none encloses it. A gate's opaque
// scope is the whole `gate[N]` subtree (generate / evaluate / until all run
// within one attempt); a map's is its `map[N].body` subtree (the per-item
// fan-out). loop bodies are NOT opaque — the "most recent iteration" rule (spec
// §5.2) keeps loop steps referenceable from outside — and neither are try /
// parallel, which introduce no multiplicity.
//
// Innermost suffices for the AWF5003 reachability check: a reference site that
// is inside the innermost scope is necessarily inside every outer one too.
func opaqueScopePrefix(staticPath string) (string, bool) {
	segs := strings.Split(staticPath, ".")
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i] == "body" && i >= 1 && strings.HasPrefix(segs[i-1], "map[") {
			return strings.Join(segs[:i+1], "."), true
		}
		if strings.HasPrefix(segs[i], "gate[") {
			return strings.Join(segs[:i+1], "."), true
		}
	}
	return "", false
}

// pathWithinScope reports whether refSite lies within the scope subtree rooted
// at scopePrefix. refSite carries a trailing field segment (e.g. ".run") but
// that only makes it deeper, so a segment-aware prefix match is exact.
func pathWithinScope(refSite, scopePrefix string) bool {
	return refSite == scopePrefix || strings.HasPrefix(refSite, scopePrefix+".")
}

// renderRef formats a Ref for inclusion in a diagnostic message.
func renderRef(r template.Ref) string {
	var b strings.Builder
	for i, s := range r.Segments {
		if i > 0 {
			b.WriteByte('.')
		}
		if s.IsIndex {
			fmt.Fprintf(&b, "%d", s.Index)
		} else {
			b.WriteString(s.Ident)
		}
	}
	return b.String()
}

// validateAwfOutputWrites emits AWF3006 (Warning) for every CodeStep that declares
// an output_schema but whose run script contains no reference to AWF_OUTPUT. Without
// a write to $AWF_OUTPUT the runtime will see an empty output file and the schema
// validation will silently succeed with a null object — a common authoring mistake.
//
// The check is intentionally surface-level (substring match, not AST analysis) because
// run: is an arbitrary shell script and a precise static analysis would be unsound. The
// heuristic catches the most common forget — the author wrote `echo hi` instead of
// `echo '{"x":1}' > "$AWF_OUTPUT"`. Surface-level false negative: a script that merely
// mentions the literal string "AWF_OUTPUT" without writing it (e.g. `echo "write to AWF_OUTPUT"`)
// also suppresses the warning — acceptable given the check's intentionally surface-level nature.
func validateAwfOutputWrites(nodes NodeList, c *collector) {
	WalkNodes(nodes, "", func(n Node, path string) {
		cs, ok := n.(*CodeStep)
		if !ok || cs.OutputSchema == nil {
			return
		}
		if !strings.Contains(cs.Run, "AWF_OUTPUT") {
			c.warnf(path, "AWF3006", catalog["AWF3006"])
		}
	})
}

// syntaxMessage extracts the inner Msg from a *template.SyntaxError if err is one; falls
// back to err.Error() otherwise. Avoids the "position N: " prefix when we're already
// attributing via a path.
func syntaxMessage(err error) string {
	var se *template.SyntaxError
	if errors.As(err, &se) {
		return se.Msg
	}
	return err.Error()
}
