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
	kind   string // "code", "agent", "signal", "input"
	schema *JSONSchema
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

	// Track which producers had at least one ref into them (for AWF3002).
	referenced := map[string]bool{}

	// Walk the graph collecting refs from every Template and Expr field. evaluateAllowed=false
	// and overSink=false at the top level — only the gate frame's generate/until flip
	// evaluateAllowed true, and only a map's over: flips overSink true (see walkRefs).
	walkRefs(wf.Graph, "", c, producers, referenced, false, false)

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

func indexProducers(nodes NodeList, producers map[string]producer) {
	WalkNodes(nodes, "", func(n Node, path string) {
		switch v := n.(type) {
		case *CodeStep:
			producers[v.ID] = producer{path: path, kind: "code", schema: v.OutputSchema}
		case *AgentStep:
			producers[v.ID] = producer{path: path, kind: "agent", schema: v.OutputSchema}
		case *SignalStep:
			producers[v.ID] = producer{path: path, kind: "signal", schema: v.OutputSchema}
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
// overSink is true ONLY inside a map's over: expression — the one array-native sink. It is
// threaded so checkRef's step case can allow an aggregate (array-typed) ref there and emit
// AWF5004 everywhere else. Every non-over call site passes false (over: is a single Expr with
// no recursion, so the flag never propagates into a subtree).
func walkRefs(nodes NodeList, parent string, c *collector, producers map[string]producer, referenced map[string]bool, evaluateAllowed, overSink bool) {
	for i, n := range nodes {
		switch v := n.(type) {
		case *CodeStep:
			path := PathFor(parent, "", v.ID, i)
			checkTemplateRefs(v.Run, path+".run", c, producers, referenced, evaluateAllowed, false)
			if v.IdempotencyKey != nil {
				checkTemplateRefs(string(*v.IdempotencyKey), path+".idempotency_key", c, producers, referenced, evaluateAllowed, false)
			}
		case *AgentStep:
			path := PathFor(parent, "", v.ID, i)
			if v.IdempotencyKey != nil {
				checkTemplateRefs(string(*v.IdempotencyKey), path+".idempotency_key", c, producers, referenced, evaluateAllowed, false)
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
					checkTemplateRefs(sv, path+".with."+k, c, producers, referenced, evaluateAllowed, false)
				}
			}
		case *SignalStep:
			// no Template / Expr fields beyond the schema itself.
		case *If:
			path := PathFor(parent, "if", "", i)
			checkExprRefs(string(v.Cond), path+".cond", c, producers, referenced, evaluateAllowed, false)
			walkRefs(v.Then, ChildPath(parent, "if", i, "then"), c, producers, referenced, evaluateAllowed, false)
			walkRefs(v.Else, ChildPath(parent, "if", i, "else"), c, producers, referenced, evaluateAllowed, false)
		case *Loop:
			path := PathFor(parent, "loop", "", i)
			if v.Until != nil {
				checkExprRefs(string(*v.Until), path+".until", c, producers, referenced, evaluateAllowed, false)
			}
			walkRefs(v.Body, ChildPath(parent, "loop", i, "body"), c, producers, referenced, evaluateAllowed, false)
		case *Try:
			walkRefs(v.Do, ChildPath(parent, "try", i, "do"), c, producers, referenced, evaluateAllowed, false)
			walkRefs(v.Catch, ChildPath(parent, "try", i, "catch"), c, producers, referenced, evaluateAllowed, false)
			walkRefs(v.Finally, ChildPath(parent, "try", i, "finally"), c, producers, referenced, evaluateAllowed, false)
		case *Parallel:
			walkRefs(v.Children, PathFor(parent, "parallel", "", i), c, producers, referenced, evaluateAllowed, false)
		case *Gate:
			path := PathFor(parent, "gate", "", i)
			// gate.until: evaluate.* allowed (single Expr field, no recursion).
			checkExprRefs(string(v.Until), path+".until", c, producers, referenced, true, false)
			// gate.generate: evaluate.* allowed (innermost frame OVERRIDES enclosing).
			walkRefs(v.Generate, ChildPath(parent, "gate", i, "generate"), c, producers, referenced, true, false)
			// gate.evaluate: evaluate.* REJECTED (the evaluator can't reference its own in-flight output).
			walkRefs(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), c, producers, referenced, false, false)
		case *Map:
			path := PathFor(parent, "map", "", i)
			// over: is the one array-native sink — an aggregate ref is legal here (overSink=true).
			checkExprRefs(string(v.Over), path+".over", c, producers, referenced, evaluateAllowed, true)
			// v.Container is a STATIC container name (AWF §5.7); validated by walkStructural
			// (AWF1009/AWF1019). Not a Template — no Slots/ParseRef walk here.
			walkRefs(v.Body, ChildPath(parent, "map", i, "body"), c, producers, referenced, evaluateAllowed, false)
		}
	}
}

// checkTemplateRefs scans src (an ir.Template field) for `{{ … }}` slots, parses each as a
// ref via the template package, and runs each through checkRef. evaluateAllowed is propagated
// to checkRef so the `evaluate.<field>` scope rule (AWF5001) can fire. overSink is propagated
// so the step case can allow an aggregate ref (and emit AWF5004 elsewhere).
func checkTemplateRefs(src, path string, c *collector, producers map[string]producer, referenced map[string]bool, evaluateAllowed, overSink bool) {
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
		checkRef(*ref, path, c, producers, referenced, evaluateAllowed, overSink)
	}
}

// checkExprRefs unwraps the outer `{{ }}` envelope (if present), parses the inner as an
// Expr via the template package, and runs each Ref in the AST through checkRef. evaluateAllowed
// is propagated to checkRef so the `evaluate.<field>` scope rule (AWF5001) can fire. overSink
// is propagated so the step case can allow an aggregate ref (only a map's over: passes true).
func checkExprRefs(src, path string, c *collector, producers map[string]producer, referenced map[string]bool, evaluateAllowed, overSink bool) {
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
		checkRef(ref, path, c, producers, referenced, evaluateAllowed, overSink)
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

// checkRef classifies a ref by its first segment and applies the appropriate cross-check.
// evaluateAllowed controls whether `evaluate.<field>` is legal in this position — false
// emits AWF5001 (the static counterpart of engine.Scope.resolveEvaluate's runtime check).
// overSink is true only inside a map's over: expression — the one array-native sink where an
// aggregate (array-typed) ref is legal; elsewhere an aggregate ref emits AWF5004.
func checkRef(ref template.Ref, path string, c *collector, producers map[string]producer, referenced map[string]bool, evaluateAllowed, overSink bool) {
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
		// Aggregate read: producer inside the v1 single-map shape, ref site OUTSIDE it.
		if mapPath, _, isAgg := SingleMapBodyShape(p.path); isAgg && !pathWithinScope(path, mapPath) {
			if !overSink {
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
