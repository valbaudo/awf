package ir

import (
	"bytes"
	"encoding/json"
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
	kind   string // "code", "agent", "signal", "call", "input", "map_reduce", "map_compact"
	schema *JSONSchema
	reduce *Reduce
}

// validateRefsModule runs the AWF3001/AWF3002 pass: walks every ir.Template and ir.Expr field,
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
func validateRefsModule(ld *LoadedDefinition, mod validationModule, c *collector) {
	wf := mod.Workflow

	// Build the producer index: step.id → (kind, output_schema). Used to check AWF3001.
	producers := map[string]producer{}
	indexModuleProducers(ld, mod.ModuleID, wf.Graph, producers)

	// Synthetic "input" producer so the same checkRef machinery can validate input.<field>
	// against wf.InputSchema. Path "input" mirrors what TestSchemaInputSchemaAlsoValidated uses.
	if wf.InputSchema != nil {
		producers["input"] = producer{path: "input", kind: "input", schema: wf.InputSchema}
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
	markTemplateValueRefsReferenced(wf, c, producers, maps, referenced)

	// AWF3002: any AgentStep with an output_schema but no inbound ref → warning.
	for id, p := range producers {
		if p.kind == "agent" && p.schema != nil && !referenced[id] {
			c.warnf(p.path, "AWF3002", fmt.Sprintf("%s (step %q)", catalog["AWF3002"], id))
		}
	}
}

func markTemplateValueRefsReferenced(wf *Workflow, c *collector, producers map[string]producer, maps map[string]*Map, referenced map[string]bool) {
	tmp := &collector{source: c.source}
	WalkNodes(wf.Graph, "", func(n Node, nodePath string) {
		call, ok := n.(*CallStep)
		if !ok {
			return
		}
		validateTemplateValueRefs(tmp, "AWF3001", nodePath+".input", call.Input, producers, maps, referenced)
	})
	if len(wf.Outputs) == 0 {
		return
	}
	keys := make([]string, 0, len(wf.Outputs))
	for key := range wf.Outputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		validateTemplateValueRefs(tmp, "AWF3001", "outputs."+key, map[string]TemplateValue{"": wf.Outputs[key]}, producers, maps, referenced)
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

// lastEvaluatorProducerID returns the id of the terminal producer of a gate's
// evaluate list — the node {{ evaluate.<field> }} resolves to. AWF1014
// (validate_structural.go) guarantees that terminal is a Code/Agent/Signal step
// with output_schema, or (jury-panel Task 2) a *Map whose reduce: produces a
// typed verdict — so the default arm is unreachable for a valid gate (it
// mirrors engine/gate.go lastEvaluatorPath and nodeHasOutputSchema). Do NOT add
// *React/*Call arms: nodeHasOutputSchema rejects them as evaluate terminals.
func lastEvaluatorProducerID(nodes NodeList) string {
	if len(nodes) == 0 {
		return ""
	}
	switch v := nodes[len(nodes)-1].(type) {
	case *CodeStep:
		return v.ID
	case *AgentStep:
		return v.ID
	case *SignalStep:
		return v.ID
	case *Map:
		// The aggregate id the verdict is addressed under — the map's reduce
		// commits its typed output at the map's own path (see lastEvaluatorPath's
		// *ir.Map arm). Marking it referenced mirrors the step arms; it has no
		// AWF3002 effect today (that check only fires for kind=="agent", and a
		// reduce-declaring map is indexed as kind=="map_reduce" — see
		// indexModuleProducers), but keeps this function's contract — every
		// evaluate terminal's id is marked referenced — true regardless.
		return v.ID
	default:
		return "" // AWF1014-unreachable for a valid gate
	}
}

func indexModuleProducers(ld *LoadedDefinition, moduleID string, nodes NodeList, producers map[string]producer) {
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
		case *CallStep:
			child, ok := callTargetModule(ld, moduleID, v.Call)
			if ok && child != nil && child.Workflow != nil {
				producers[v.ID] = producer{path: path, ord: currentOrd, kind: "call", schema: callProducerSchema(child.Workflow)}
			} else {
				producers[v.ID] = producer{path: path, ord: currentOrd, kind: "call"}
			}
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
		case *React:
			// React's output is addressable by id as `{{ <id>.* }}` (P3 §3.2/§6.1) — the
			// react id is the ROOT segment, unlike step.<id>; the default arm of checkRef
			// resolves a kind=="react" producer keyed by that root ident.
			//
			// Multiplicity (pinned, N3): a TOP-LEVEL react is fully referenceable via
			// `{{ <id>.* }}`; a react nested inside loop/gate/map is `--step`-only (NOT
			// direct-addressable), the same boundary Map's product addressing carries. A
			// top-level react has the bare runtime path "react[N]" (no parent segment); a
			// nested one is e.g. "loop[0].body.react[0]". Register ONLY at top level so a
			// nested ref falls through checkRef's default arm (deferred to the runtime
			// evaluator scope), exactly as today.
			if v.ID == "" || strings.Contains(path, ".") {
				return
			}
			producers[v.ID] = producer{path: path, ord: currentOrd, kind: "react", schema: v.OutputSchema}
		}
	})
}

func callProducerSchema(wf *Workflow) *JSONSchema {
	if wf == nil || wf.OutputSchema == nil || len(wf.Outputs) == 0 {
		return nil
	}
	props, _ := (*wf.OutputSchema)["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	boundProps := map[string]any{}
	keys := make([]string, 0, len(wf.Outputs))
	for key := range wf.Outputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if prop, ok := props[key]; ok {
			boundProps[key] = prop
		}
	}
	if len(boundProps) == 0 {
		return nil
	}
	schema := JSONSchema{
		"type":                 "object",
		"properties":           boundProps,
		"additionalProperties": false,
	}
	return &schema
}

func callTargets(ld *LoadedDefinition, parentID string) map[string]string {
	out := map[string]string{}
	if ld == nil {
		return out
	}
	_ = ld.WalkImportEdges(func(edge LoadedImportEdge) error {
		if edge.ParentID == parentID {
			out[edge.ImportID] = edge.ChildID
		}
		return nil
	})
	return out
}

func callTargetModule(ld *LoadedDefinition, parentID, importID string) (*LoadedModule, bool) {
	targets := callTargets(ld, parentID)
	childID, ok := targets[importID]
	if !ok {
		return nil, false
	}
	return ld.Module(childID)
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
			checkShellHostInjection(v.Run, path+".run", c, producers)
			if v.IdempotencyKey != nil {
				checkTemplateRefs(string(*v.IdempotencyKey), path+".idempotency_key", c, producers, maps, referenced, evaluateAllowed, "")
				checkShellHostInjection(string(*v.IdempotencyKey), path+".idempotency_key", c, producers)
			}
		case *AgentStep:
			path := PathFor(parent, "", v.ID, i)
			if v.IdempotencyKey != nil {
				checkTemplateRefs(string(*v.IdempotencyKey), path+".idempotency_key", c, producers, maps, referenced, evaluateAllowed, "")
				checkShellHostInjection(string(*v.IdempotencyKey), path+".idempotency_key", c, producers)
			}
			if v.Skills != nil {
				checkFieldSize(string(v.Skills.Query), path+".skills.query", c)
				checkTemplateRefs(string(v.Skills.Query), path+".skills.query", c, producers, maps, referenced, evaluateAllowed, "")
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
			// The evaluator's terminal typed output is consumed via
			// evaluate.<field> (no step id); mark it referenced so AWF3002 does
			// not flag it. Terminal is always a producer step (AWF1014). P12.
			if id := lastEvaluatorProducerID(v.Evaluate); id != "" {
				referenced[id] = true
			}
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
	walkTemplateRefs(src, path, c, func(ref template.Ref) {
		checkReduceRef(ref, path, c, producers, maps, referenced, evaluateAllowed)
	})
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
	walkTemplateRefs(src, path, c, func(ref template.Ref) {
		checkRef(ref, path, c, producers, maps, referenced, evaluateAllowed, overSinkMapPath)
	})
}

func walkTemplateRefs(src, path string, c *collector, visit func(template.Ref)) {
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
		visit(*ref)
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

// QuorumVerdictFields is the FIELD-INDEPENDENT part of the fixed typed-output
// shape a quorum reducer commits: every quorum verdict carries these three keys
// PLUS one field-named key (the reduce's own `field:`, e.g. `vulnerable`) holding
// the pass/fail bool. It is the SINGLE source of truth for that fixed part of the
// contract: this validator binds downstream refs against it (a ref accepted iff
// it names one of these OR the reduce's own field:), and engine's
// runQuorumReduce must produce EXACTLY these keys plus the field-named key —
// pinned by a cross-package engine test (TestQuorumReduceOutputMatchesVerdictFields)
// so the producer (engine/reduce.go) and the validator can never silently drift.
// Exported only for that test.
var QuorumVerdictFields = map[string]bool{"votes": true, "agree": true, "votes_detail": true}

// checkReducedMapRef validates a `step.<bodyId>[.<field>]` reference into a map that
// declares a reduce:. The ref resolves against the REDUCER's committed output (the
// runtime's LookupCompleted(mapStatic)), NOT the per-item aggregate — so:
//
//   - 2-seg `step.<bodyId>` → the reducer's whole output (a scalar object); accepted.
//   - 3-seg `step.<bodyId>.<field>`:
//   - run: reducer → <field> must be declared in Reduce.OutputSchema (AWF3001 else).
//   - quorum reducer → <field> must be the reduce's own `field:` (the verdict
//     bool) or one of {votes, agree, votes_detail} (AWF3001 else).
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
		if field != r.Field && !QuorumVerdictFields[field] {
			c.errf(path, "AWF3001", fmt.Sprintf("step %q is a quorum-reduced map; the reduced verdict declares only {%s, votes, agree, votes_detail}, not field %q", id, r.Field, field))
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
		// AWF5003 / AWF5002: a MAP body is an opaque multiplicity scope — a producer
		// inside one resolves only from within the same item, because from outside
		// there are N items and no single instance to bind to. A GATE is not: a
		// passed gate has exactly one accepted attempt, so it is transparent to its
		// generate: subtree and blockingScope peels it. loop / try / parallel are
		// transparent too (loops via the "most recent iteration" rule). The
		// single-map aggregate shape was handled above; a still-unmatched scope
		// here means a gate EVALUATOR read from outside (verdict is gate-internal),
		// a gate nested inside a map body (per-item accepted attempts fan in only
		// via reduce:), or a genuinely nested/loop-multiplied map (aggregation
		// deferred). This is the static counterpart of
		// engine.Scope.stepRuntimePath's map/gate arms.
		if scope, peeledGate, blocked := blockingScope(p.path, path); blocked {
			// A scope between the producer and the reference site blocks it.
			// Key the choice off the BLOCKING scope AND whether a gate was
			// peeled en route to it — not off whether the producer's full path
			// merely contains "gate[":
			//   - blocking scope is a gate → the reference reached a gate via
			//     evaluate: (AWF5003 — the evaluator's verdict stays internal).
			//   - blocking scope is a map and a gate was peeled first → the
			//     producer is a gate nested inside that map body (AWF5003 — it
			//     binds only through reduce:).
			//   - blocking scope is a map and no gate was peeled → the map
			//     itself is genuinely nested or loop-multiplied (AWF5002 —
			//     aggregation across those is not yet defined).
			if isGateScope(scope) || peeledGate {
				c.errf(path, "AWF5003", fmt.Sprintf("%s: %s", catalog["AWF5003"], renderRef(ref)))
				return
			}
			c.errf(path, "AWF5002", fmt.Sprintf("%s: %s", catalog["AWF5002"], renderRef(ref)))
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
		// react: the engine injects a reserved `stop_reason` sibling that the
		// output_schema does NOT (and may not, AWF1055) declare. Accept it as a
		// synthetic field so `{{ <id>.stop_reason }}` resolves statically even though
		// it isn't in the schema — it resolves fine at runtime via descendPath (§5/§6.1).
		if p.kind == "react" && reactReservedField(field) {
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
		// A react: node's output is addressed by id as the ROOT segment —
		// `{{ <id>.<field> }}` / `{{ <id>.stop_reason }}` (P3 §3.2/§6.1), NOT
		// `step.<id>`. If root names a registered top-level react producer, validate
		// the field against its output_schema plus the synthetic stop_reason sibling.
		if p, ok := producers[root]; ok && p.kind == "react" {
			if len(ref.Segments) < 2 || ref.Segments[1].IsIndex {
				c.errf(path, "AWF3001", fmt.Sprintf("malformed react reference (need %s.<field>): %s", root, renderRef(ref)))
				return
			}
			field := ref.Segments[1].Ident
			if reactReservedField(field) {
				referenced[root] = true
				return
			}
			if !checkSchemaField(c, path, root, field, p.schema) {
				return
			}
			referenced[root] = true
			return
		}
		// Unknown root (e.g. an `<as>` binding from a map) — slice 1.4 doesn't track binding
		// scopes; defer to Phase 2's evaluator scope check.
	}
}

// reactReservedField reports whether field is an engine-injected react output
// sibling (currently just stop_reason) — accepted in `{{ <react-id>.<field> }}`
// refs even though output_schema does not (and may not, AWF1055) declare it.
// reservedReactOutputFields (validate_tools.go) is the single source of truth.
func reactReservedField(field string) bool {
	for _, r := range reservedReactOutputFields {
		if r == field {
			return true
		}
	}
	return false
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

// blockingScope walks producerPath's opaque scopes from innermost outward and
// returns the first scope that BLOCKS a reference sited at refSite. blocked ==
// false means no scope blocks, i.e. the reference resolves.
//
// A gate scope is PEELED when the producer sits in that gate's generate:
// subtree — a passed gate has exactly one accepted attempt, so it is transparent
// there (the runtime counterpart is engine.Scope.stepRuntimePath's
// accepted-attempt fallback). A gate reached via evaluate: BLOCKS: the judge's
// verdict is gate-internal by design, and exposing it would let a workflow
// branch on a condition that is true by construction. A map body always blocks:
// N items, no single instance to bind to.
//
// peeledGate reports whether a gate scope was peeled (and the walk continued
// outward) before landing on the returned blocking scope. The call site needs
// this: when the blocking scope is a MAP, "a gate was peeled first" means the
// producer is a gate nested inside that map body (per-item accepted attempts
// fan in only via reduce: → AWF5003), while "no gate was peeled" means the map
// itself is the reason the walk never resolved — genuinely nested or
// loop-multiplied maps (aggregation deferred → AWF5002). blockingScope is the
// only thing that knows which case it is; opaqueScopePrefix alone can't tell,
// since it returns only the innermost opaque scope.
//
// Walking OUTWARD is load-bearing. opaqueScopePrefix returns only the INNERMOST
// opaque scope, so for a producer at "map[0].body.gate[0].generate.x" it returns
// the gate. Stopping there would let a reference from outside the MAP validate
// clean and silently reopen map opacity through a gate.
func blockingScope(producerPath, refSite string) (scope string, peeledGate bool, blocked bool) {
	rest := producerPath
	peeled := false
	for {
		sc, opaque := opaqueScopePrefix(rest)
		if !opaque || pathWithinScope(refSite, sc) {
			return "", false, false
		}
		if !isGateScope(sc) {
			return sc, peeled, true // a map body the reference site is outside of
		}
		// A gate[N] scope. Transparent to generate: only; scope is always a
		// prefix of producerPath, so the segment after it names the branch.
		if !strings.HasPrefix(strings.TrimPrefix(producerPath, sc+"."), "generate.") {
			return sc, peeled, true
		}
		peeled = true
		i := strings.LastIndexByte(sc, '.')
		if i < 0 {
			return "", false, false // top-level gate; nothing further out
		}
		rest = sc[:i]
	}
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

// checkShellHostInjection emits AWF3013 (Warning) for every `{{ }}` slot in src
// that resolves to a string-typed field and is not already wrapped in shell quotes.
//
// Shell hosts (run: and idempotency_key) substitute slots verbatim before the shell
// sees the command (template.Substitute → raw string). An attacker-controlled string
// value is therefore directly injectable (CWE-78 / GitHub Actions ${{ }}-into-run:
// class). Number/boolean/integer fields are shell-safe (they render via strconv with
// no shell-special chars) and are skipped. A slot that is already surrounded by `"`
// or `'` in the host string is also skipped.
//
// The quote detection is deliberately surface-level (byte-offset heuristic on the
// immediately adjacent char, NOT AST analysis) — mirroring validateAwfOutputWrites's
// "substring match, not AST analysis" stance. A residual false-negative on exotic
// quoting (e.g. a variable expansion that happens to end in a quote char) is
// accepted.
func checkShellHostInjection(src, path string, c *collector, producers map[string]producer) {
	slots, err := template.Slots(src)
	if err != nil {
		return // parse errors are already surfaced by checkTemplateRefs / walkTemplateRefs
	}
	for _, sl := range slots {
		inner := strings.TrimSpace(sl.Inner)
		ref, err := template.ParseRef(inner)
		if err != nil || ref == nil {
			continue // malformed refs are handled by checkTemplateRefs
		}
		if !refIsStringTyped(*ref, producers) {
			continue // non-string (integer, boolean, …) fields are shell-safe
		}
		if slotIsShellQuoted(src, sl) {
			continue // already inside double or single quotes — safe
		}
		c.warnf(path, "AWF3013", catalog["AWF3013"])
	}
}

// refIsStringTyped reports whether ref resolves to a declared string-typed field.
// It only returns true for the cases that matter for shell injection:
//   - step.<id>.<field> where the producer's output_schema declares field with type=="string"
//   - input.<field> where the workflow input schema declares field with type=="string"
//
// exit_code and stdout are integer/string respectively; exit_code short-circuits to
// false (integer, safe). stdout is string-typed, so it will return true — the author
// should quote `"{{ step.x.stdout }}"` to avoid injection.
// Any ref that doesn't match these two forms (run.id, evaluate.*, unknown root) is
// skipped (returns false) so we never warn on refs the engine itself controls.
func refIsStringTyped(ref template.Ref, producers map[string]producer) bool {
	if len(ref.Segments) == 0 {
		return false
	}
	root := ref.Segments[0].Ident
	switch root {
	case "step":
		if len(ref.Segments) < 3 || ref.Segments[1].IsIndex || ref.Segments[2].IsIndex {
			return false
		}
		id := ref.Segments[1].Ident
		field := ref.Segments[2].Ident
		// exit_code is integer — shell-safe.
		if field == "exit_code" {
			return false
		}
		// stdout is string — shell-injectable.
		if field == "stdout" {
			return true
		}
		p, ok := producers[id]
		if !ok || p.schema == nil {
			return false
		}
		props, _ := (*p.schema)["properties"].(map[string]any)
		prop, ok := props[field]
		if !ok {
			return false
		}
		spec, _ := prop.(map[string]any)
		t, _ := spec["type"].(string)
		return t == "string"
	case "input":
		if len(ref.Segments) < 2 || ref.Segments[1].IsIndex {
			return false
		}
		field := ref.Segments[1].Ident
		p, ok := producers["input"]
		if !ok || p.schema == nil {
			return false
		}
		props, _ := (*p.schema)["properties"].(map[string]any)
		prop, ok := props[field]
		if !ok {
			return false
		}
		spec, _ := prop.(map[string]any)
		t, _ := spec["type"].(string)
		return t == "string"
	}
	return false
}

// slotIsShellQuoted reports whether sl is immediately surrounded by shell quote chars
// (`"` or `'`) in the host string. Specifically: the byte at host[sl.Start-1] must
// equal the byte at host[sl.End] and both must be `"` or `'`.
//
// This is a deliberately surface-level heuristic (not a shell AST analysis). It
// correctly identifies the blessed pattern `"{{ step.x.url }}"` documented in
// container/backend.go. A residual false-negative on exotic quoting (e.g. a `"`
// preceded by a variable expansion) is accepted.
func slotIsShellQuoted(host string, sl template.Slot) bool {
	if sl.Start == 0 || sl.End >= len(host) {
		return false
	}
	before := host[sl.Start-1]
	after := host[sl.End]
	return (before == '"' && after == '"') || (before == '\'' && after == '\'')
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

func validateCalls(ld *LoadedDefinition, mod validationModule, c *collector) {
	wf := mod.Workflow
	targets := callTargets(ld, mod.ModuleID)
	producers := map[string]producer{}
	indexModuleProducers(ld, mod.ModuleID, wf.Graph, producers)
	if wf.InputSchema != nil {
		producers["input"] = producer{path: "input", kind: "input", schema: wf.InputSchema}
	}
	order := nodeOrder(wf.Graph)
	outFiles := outputFilesByStepIDForModule(ld, mod.ModuleID, wf)
	maps := mapsByPath(wf.Graph)

	WalkNodes(wf.Graph, "", func(n Node, nodePath string) {
		call, ok := n.(*CallStep)
		if !ok {
			return
		}
		if call.Call == "" {
			c.errf(nodePath, "AWF1046", catalog["AWF1046"]+": empty call target")
		} else if _, ok := targets[call.Call]; !ok {
			c.errf(nodePath, "AWF1046", fmt.Sprintf("%s: %q", catalog["AWF1046"], call.Call))
		} else if child, ok := callTargetModule(ld, mod.ModuleID, call.Call); ok && child != nil && child.Workflow != nil {
			validateCallInputContract(c, nodePath, call.Input, child.Workflow.InputSchema)
			validateCallInputFiles(ld, mod.ModuleID, c, nodePath, call.InputFiles, child.Workflow.InputFiles, producers, order, outFiles, maps)
		}
		validateTemplateValueRefs(c, "AWF1047", nodePath+".input", call.Input, producers, maps, nil)
	})
}

func validateCallInputFiles(
	ld *LoadedDefinition,
	moduleID string,
	c *collector,
	nodePath string,
	inputFiles map[string]string,
	childInputFiles WorkflowInputFiles,
	producers map[string]producer,
	order map[string]int,
	outFiles map[string]OutputFiles,
	maps map[string]*Map,
) {
	inputPath := nodePath + ".input_files"
	childKeys := make([]string, 0, len(childInputFiles))
	for name := range childInputFiles {
		childKeys = append(childKeys, name)
	}
	sort.Strings(childKeys)
	for _, name := range childKeys {
		if _, ok := inputFiles[name]; !ok {
			c.errf(inputPath+"."+name, "AWF1051", fmt.Sprintf("%s: missing required input file %q", catalog["AWF1051"], name))
		}
	}

	parent, _ := ld.Module(moduleID)
	assets := map[string]string(nil)
	parentInputFiles := WorkflowInputFiles(nil)
	if parent != nil && parent.Workflow != nil {
		assets = parent.Workflow.Assets
		parentInputFiles = parent.Workflow.InputFiles
	}
	inputKeys := make([]string, 0, len(inputFiles))
	for name := range inputFiles {
		inputKeys = append(inputKeys, name)
	}
	sort.Strings(inputKeys)
	for _, name := range inputKeys {
		raw := inputFiles[name]
		path := inputPath + "." + name
		if _, ok := childInputFiles[name]; !ok {
			c.errf(path, "AWF1051", fmt.Sprintf("%s: child workflow input_files does not declare %q", catalog["AWF1051"], name))
		}
		validateInputFileRef(c, path, nodePath, "input_files."+name, raw, parentInputFiles, assets, producers, order, outFiles, maps)
		if id, ok := template.ParseAssetRef(raw); ok && parent != nil {
			if asset, loaded := parent.Assets[id]; loaded && asset.IsDir {
				c.errf(path, "AWF1051", fmt.Sprintf("%s: asset %s is a directory; call input_files require a file artifact", catalog["AWF1051"], id))
			}
		}
	}
}

func validateCallInputContract(c *collector, nodePath string, input map[string]TemplateValue, schema *JSONSchema) {
	inputPath := nodePath + ".input"
	if schema == nil {
		if len(input) > 0 {
			c.errf(inputPath, "AWF1047", catalog["AWF1047"]+": child workflow declares no input schema")
		}
		return
	}
	if schemaType(schema) != "" && schemaType(schema) != "object" {
		c.errf(inputPath, "AWF1047", catalog["AWF1047"]+": child workflow input schema must describe an object")
		return
	}
	required := schemaRequired(schema)
	for _, name := range required {
		if _, ok := input[name]; !ok {
			c.errf(inputPath, "AWF1047", fmt.Sprintf("%s: missing required input %q", catalog["AWF1047"], name))
		}
	}
	props, _ := (*schema)["properties"].(map[string]any)
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		prop, known := props[key]
		if !known {
			if ap, ok := (*schema)["additionalProperties"].(bool); ok && !ap {
				c.errf(inputPath+"."+key, "AWF1047", fmt.Sprintf("%s: child workflow input schema does not declare %q", catalog["AWF1047"], key))
			}
			continue
		}
		value, ok := decodeStaticTemplateValue(input[key])
		if !ok {
			c.errf(inputPath+"."+key, "AWF1047", fmt.Sprintf("%s: invalid JSON value", catalog["AWF1047"]))
			continue
		}
		if isTemplatedString(value) {
			continue
		}
		if !staticValueMatchesSchemaType(value, prop) {
			c.errf(inputPath+"."+key, "AWF1047", fmt.Sprintf("%s: input %q has incompatible static JSON type", catalog["AWF1047"], key))
		}
	}
}

func schemaType(schema *JSONSchema) string {
	if schema == nil {
		return ""
	}
	t, _ := (*schema)["type"].(string)
	return t
}

func schemaRequired(schema *JSONSchema) []string {
	if schema == nil {
		return nil
	}
	raw, _ := (*schema)["required"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func decodeStaticTemplateValue(raw TemplateValue) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

func isTemplatedString(value any) bool {
	s, ok := value.(string)
	return ok && (strings.Contains(s, "{{") || strings.Contains(s, "}}"))
}

func staticValueMatchesSchemaType(value any, prop any) bool {
	spec, ok := prop.(map[string]any)
	if !ok {
		return true
	}
	t, _ := spec["type"].(string)
	if t == "" {
		return true
	}
	switch t {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := n.Int64()
		return err == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}

// outputStepRefs returns the step ids referenced by an output's value, using the
// REAL template parser (walkTemplateRefs), NOT a regex — slot-aware (ignores
// literal text outside {{ }}) and matches the ref grammar checkRef uses. Recurses
// into arrays/objects. Each step id is returned at most once (deduped). Used only
// for the AWF3012 conditional-scope WARNING.
func outputStepRefs(tv TemplateValue) []string {
	var decoded any
	if err := json.Unmarshal(tv, &decoded); err != nil {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			walkTemplateRefs(x, "", &collector{}, func(ref template.Ref) {
				if len(ref.Segments) >= 2 && ref.Segments[0].Ident == "step" && !ref.Segments[1].IsIndex {
					id := ref.Segments[1].Ident
					if !seen[id] {
						seen[id] = true
						ids = append(ids, id)
					}
				}
			})
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(decoded)
	return ids
}

// conditionallyScoped reports whether a producer's STATIC path lies inside a
// TRANSPARENT-but-conditionally-reached scope — `if` (branch may not be taken) or
// `loop` (zero iterations) — where a top-level output ref validates CLEAN today
// yet may not commit at runtime. It EXCLUDES gate[ and map[: those are OPAQUE
// scopes whose cross-scope refs already hard-error (AWF5003 / AWF5004).
func conditionallyScoped(staticPath string) bool {
	for _, seg := range strings.Split(staticPath, ".") {
		if strings.HasPrefix(seg, "if[") || strings.HasPrefix(seg, "loop[") {
			return true
		}
	}
	return false
}

func validateWorkflowExports(ld *LoadedDefinition, mod validationModule, c *collector) {
	wf := mod.Workflow
	producers := map[string]producer{}
	indexModuleProducers(ld, mod.ModuleID, wf.Graph, producers)
	if wf.InputSchema != nil {
		producers["input"] = producer{path: "input", kind: "input", schema: wf.InputSchema}
	}
	maps := mapsByPath(wf.Graph)

	if len(wf.Outputs) > 0 && wf.OutputSchema == nil {
		c.errf("outputs", "AWF1048", catalog["AWF1048"]+": outputs require output_schema")
	}
	if wf.OutputSchema != nil {
		for _, key := range schemaRequired(wf.OutputSchema) {
			if _, ok := wf.Outputs[key]; !ok {
				c.errf("outputs."+key, "AWF1048", fmt.Sprintf("%s: required output %q has no binding", catalog["AWF1048"], key))
			}
		}
	}
	outputKeys := make([]string, 0, len(wf.Outputs))
	for key := range wf.Outputs {
		outputKeys = append(outputKeys, key)
	}
	sort.Strings(outputKeys)
	for _, key := range outputKeys {
		path := "outputs." + key
		if wf.OutputSchema != nil {
			props, _ := (*wf.OutputSchema)["properties"].(map[string]any)
			if _, ok := props[key]; !ok {
				c.errf(path, "AWF1048", fmt.Sprintf("%s: output %q is not declared in output_schema", catalog["AWF1048"], key))
			}
		}
		validateTemplateValueRefs(c, "AWF1048", path, map[string]TemplateValue{"": wf.Outputs[key]}, producers, maps, nil)
		for _, refID := range outputStepRefs(wf.Outputs[key]) {
			if p, ok := producers[refID]; ok && conditionallyScoped(p.path) {
				c.warnf(path, "AWF3012", fmt.Sprintf("%s: output %q binds step %q in conditional scope %s; if that branch is not taken the output key is omitted, and if the field is required by output_schema, validation fails", catalog["AWF3012"], key, refID, p.path))
			}
		}
	}

	if len(wf.ArtifactExports) == 0 {
		return
	}
	order := nodeOrder(wf.Graph)
	outFiles := outputFilesByStepIDForModule(ld, mod.ModuleID, wf)
	exportKeys := make([]string, 0, len(wf.ArtifactExports))
	for key := range wf.ArtifactExports {
		exportKeys = append(exportKeys, key)
	}
	sort.Strings(exportKeys)
	for _, key := range exportKeys {
		path := "output_files." + key
		if !stepIDPattern.MatchString(key) {
			c.errf(path, "AWF1049", "workflow output_files."+key+": name must match "+stepIDPattern.String())
			continue
		}
		tmp := &collector{source: c.source}
		validateNamedArtifactRef(tmp, path, "output_files."+key, wf.ArtifactExports[key], producers, order, outFiles, maps)
		reemitDiagnosticsAs(c, tmp.out, "AWF1049")
	}
}

func validateTemplateValueRefs(
	c *collector,
	code, basePath string,
	values map[string]TemplateValue,
	producers map[string]producer,
	maps map[string]*Map,
	referenced map[string]bool,
) {
	if len(values) == 0 {
		return
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := basePath
		if key != "" {
			path += "." + key
		}
		var decoded any
		if err := json.Unmarshal(values[key], &decoded); err != nil {
			c.errf(path, code, fmt.Sprintf("%s: invalid JSON value: %s", catalog[code], err))
			continue
		}
		walkTemplateValueStrings(c, code, path, decoded, producers, maps, referenced)
	}
}

func walkTemplateValueStrings(
	c *collector,
	code, path string,
	value any,
	producers map[string]producer,
	maps map[string]*Map,
	referenced map[string]bool,
) {
	switch v := value.(type) {
	case string:
		tmp := &collector{source: c.source}
		if referenced == nil {
			referenced = map[string]bool{}
		}
		checkTemplateRefs(v, path, tmp, producers, maps, referenced, false, "")
		reemitDiagnosticsAs(c, tmp.out, code)
	case []any:
		for i, elem := range v {
			walkTemplateValueStrings(c, code, fmt.Sprintf("%s[%d]", path, i), elem, producers, maps, referenced)
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			walkTemplateValueStrings(c, code, path+"."+key, v[key], producers, maps, referenced)
		}
	}
}

func reemitDiagnosticsAs(c *collector, diags []Diagnostic, code string) {
	for _, d := range diags {
		if d.Severity != Error {
			continue
		}
		c.errf(d.Path, code, fmt.Sprintf("%s: %s", catalog[code], d.Message))
	}
}
