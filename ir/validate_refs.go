package ir

import (
	"errors"
	"fmt"
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

	// Slice 3.4: also index map runtime paths so checkRef can emit AWF5002 for aggregation
	// refs (deferred per spec §11 item 4). The runtime ships per-item dispatch + commits
	// but NOT aggregated downstream output addressing.
	mapIDs := map[string]bool{}
	indexMapIDs(wf.Graph, mapIDs)

	// Track which producers had at least one ref into them (for AWF3002).
	referenced := map[string]bool{}

	// Walk the graph collecting refs from every Template and Expr field. evaluateAllowed=false
	// at the top level — only the gate frame's generate/until flip it true (see walkRefs).
	walkRefs(wf.Graph, "", c, producers, referenced, mapIDs, false)

	// AWF3002: any AgentStep with an output_schema but no inbound ref → warning.
	for id, p := range producers {
		if p.kind == "agent" && p.schema != nil && !referenced[id] {
			c.warnf(p.path, "AWF3002", fmt.Sprintf("%s (step %q)", catalog["AWF3002"], id))
		}
	}
}

// indexMapIDs walks wf.Graph and records every Map node's runtime path — these are the
// addresses that `step.<map_id>.items[*]` / `.summary` refs claim to read. Slice 3.4 uses
// this set to emit AWF5002 (aggregation refs deferred per spec §11 item 4).
//
// NB: Maps don't have IDs in the IR (only step kinds do). The "map id" an author writes
// in a ref is the map's RUNTIME PATH LEAF NAME — i.e. "map" (the leaf of "map[0]" or
// "loop[0].body.map[1]"). isMapID strips the trailing `[N]` index to extract the leaf
// kind-name for matching.
func indexMapIDs(nodes NodeList, mapIDs map[string]bool) {
	WalkNodes(nodes, "", func(n Node, path string) {
		if _, ok := n.(*Map); ok {
			mapIDs[path] = true
		}
	})
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
// mapIDs is the set of map runtime paths (built by indexMapIDs); threaded through so
// checkRef's step case can detect aggregation refs and emit AWF5002 (slice 3.4 / spec §11).
func walkRefs(nodes NodeList, parent string, c *collector, producers map[string]producer, referenced map[string]bool, mapIDs map[string]bool, evaluateAllowed bool) {
	for i, n := range nodes {
		switch v := n.(type) {
		case *CodeStep:
			path := PathFor(parent, "", v.ID, i)
			checkTemplateRefs(v.Run, path+".run", c, producers, referenced, mapIDs, evaluateAllowed)
			if v.IdempotencyKey != nil {
				checkTemplateRefs(string(*v.IdempotencyKey), path+".idempotency_key", c, producers, referenced, mapIDs, evaluateAllowed)
			}
		case *AgentStep:
			path := PathFor(parent, "", v.ID, i)
			if v.IdempotencyKey != nil {
				checkTemplateRefs(string(*v.IdempotencyKey), path+".idempotency_key", c, producers, referenced, mapIDs, evaluateAllowed)
			}
			// v.With is opaque RawConfig per CLAUDE.md — do NOT walk it.
		case *SignalStep:
			// no Template / Expr fields beyond the schema itself.
		case *If:
			path := PathFor(parent, "if", "", i)
			checkExprRefs(string(v.Cond), path+".cond", c, producers, referenced, mapIDs, evaluateAllowed)
			walkRefs(v.Then, ChildPath(parent, "if", i, "then"), c, producers, referenced, mapIDs, evaluateAllowed)
			walkRefs(v.Else, ChildPath(parent, "if", i, "else"), c, producers, referenced, mapIDs, evaluateAllowed)
		case *Loop:
			path := PathFor(parent, "loop", "", i)
			if v.Until != nil {
				checkExprRefs(string(*v.Until), path+".until", c, producers, referenced, mapIDs, evaluateAllowed)
			}
			walkRefs(v.Body, ChildPath(parent, "loop", i, "body"), c, producers, referenced, mapIDs, evaluateAllowed)
		case *Try:
			walkRefs(v.Do, ChildPath(parent, "try", i, "do"), c, producers, referenced, mapIDs, evaluateAllowed)
			walkRefs(v.Catch, ChildPath(parent, "try", i, "catch"), c, producers, referenced, mapIDs, evaluateAllowed)
			walkRefs(v.Finally, ChildPath(parent, "try", i, "finally"), c, producers, referenced, mapIDs, evaluateAllowed)
		case *Parallel:
			walkRefs(v.Children, PathFor(parent, "parallel", "", i), c, producers, referenced, mapIDs, evaluateAllowed)
		case *Gate:
			path := PathFor(parent, "gate", "", i)
			// gate.until: evaluate.* allowed (single Expr field, no recursion).
			checkExprRefs(string(v.Until), path+".until", c, producers, referenced, mapIDs, true)
			// gate.generate: evaluate.* allowed (innermost frame OVERRIDES enclosing).
			walkRefs(v.Generate, ChildPath(parent, "gate", i, "generate"), c, producers, referenced, mapIDs, true)
			// gate.evaluate: evaluate.* REJECTED (the evaluator can't reference its own in-flight output).
			walkRefs(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), c, producers, referenced, mapIDs, false)
		case *Map:
			path := PathFor(parent, "map", "", i)
			checkExprRefs(string(v.Over), path+".over", c, producers, referenced, mapIDs, evaluateAllowed)
			// v.Container is a STATIC container name (AWF §5.7); validated by walkStructural
			// (AWF1009/AWF1019). Not a Template — no Slots/ParseRef walk here.
			walkRefs(v.Body, ChildPath(parent, "map", i, "body"), c, producers, referenced, mapIDs, evaluateAllowed)
		}
	}
}

// checkTemplateRefs scans src (an ir.Template field) for `{{ … }}` slots, parses each as a
// ref via the template package, and runs each through checkRef. evaluateAllowed is propagated
// to checkRef so the `evaluate.<field>` scope rule (AWF5001) can fire. mapIDs is propagated
// so the step case can emit AWF5002 for aggregation refs.
func checkTemplateRefs(src, path string, c *collector, producers map[string]producer, referenced map[string]bool, mapIDs map[string]bool, evaluateAllowed bool) {
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
		checkRef(*ref, path, c, producers, referenced, mapIDs, evaluateAllowed)
	}
}

// checkExprRefs unwraps the outer `{{ }}` envelope (if present), parses the inner as an
// Expr via the template package, and runs each Ref in the AST through checkRef. evaluateAllowed
// is propagated to checkRef so the `evaluate.<field>` scope rule (AWF5001) can fire. mapIDs
// is propagated so the step case can emit AWF5002 for aggregation refs.
func checkExprRefs(src, path string, c *collector, producers map[string]producer, referenced map[string]bool, mapIDs map[string]bool, evaluateAllowed bool) {
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
		checkRef(ref, path, c, producers, referenced, mapIDs, evaluateAllowed)
	}
}

// checkRef classifies a ref by its first segment and applies the appropriate cross-check.
// evaluateAllowed controls whether `evaluate.<field>` is legal in this position — false
// emits AWF5001 (the static counterpart of engine.Scope.resolveEvaluate's runtime check).
// mapIDs is consulted in the step case to emit AWF5002 for `step.<map>.items[*]` /
// `step.<map>.summary.<field>` aggregation refs (deferred per spec §11 item 4).
func checkRef(ref template.Ref, path string, c *collector, producers map[string]producer, referenced map[string]bool, mapIDs map[string]bool, evaluateAllowed bool) {
	if len(ref.Segments) == 0 {
		return
	}
	root := ref.Segments[0].Ident
	switch root {
	case "step":
		// step.<id>.<field> — require at least 3 segments (step, id, field).
		if len(ref.Segments) < 3 || ref.Segments[1].IsIndex || ref.Segments[2].IsIndex {
			c.errf(path, "AWF3001", fmt.Sprintf("malformed step reference (need step.<id>.<field>): %s", renderRef(ref)))
			return
		}
		id := ref.Segments[1].Ident
		field := ref.Segments[2].Ident
		// Slice 3.4 HI-A precedence: AWF5002 fires ONLY when (a) the id is NOT a known step
		// producer AND (b) it matches a known map leaf-name. This ordering prevents AWF5002
		// from tripping on a legit `step.foo.items` ref when `foo` is a real step (whose
		// schema may declare `items`) and `foo` also happens to share a leaf-name with a
		// Map kind in the same workflow. The step-producer branch wins; AWF3001 below
		// handles the case where `items` isn't in the step's schema with its clear
		// "field not declared" message.
		if _, isStep := producers[id]; !isStep && isMapID(id, mapIDs) {
			if field == "items" || field == "summary" {
				c.errf(path, "AWF5002", fmt.Sprintf("%s: %s", catalog["AWF5002"], renderRef(ref)))
				return
			}
		}
		p, ok := producers[id]
		if !ok {
			c.errf(path, "AWF3001", fmt.Sprintf("reference to undeclared step %q", id))
			return
		}
		// AWF5003: gate/map bodies are opaque multiplicity scopes. A step inside
		// one resolves only from within the same attempt/item (structurally: the
		// reference site must be inside the producer's enclosing gate/map subtree).
		// A reference from outside has no defined attempt/item and the runtime
		// rejects it — the static counterpart of engine.Scope.stepRuntimePath's
		// same-attempt/same-item check. loop / try / parallel are transparent
		// (loops via the "most recent iteration" rule) and don't trigger this.
		if scope, opaque := opaqueScopePrefix(p.path); opaque && !pathWithinScope(path, scope) {
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
		if p.schema == nil {
			c.errf(path, "AWF3001", fmt.Sprintf("reference to step %q field %q but no output_schema declared", id, field))
			return
		}
		props, _ := (*p.schema)["properties"].(map[string]any)
		if _, ok := props[field]; !ok {
			c.errf(path, "AWF3001", fmt.Sprintf("step %q output_schema does not declare field %q", id, field))
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

// isMapID reports whether name matches the LEAF of any known map runtime path.
// E.g. mapIDs contains paths like "map[0]" and "loop[0].body.map[1]" — for an author
// writing `step.map.items[0]`, the leaf-name "map" matches the leaf of "map[0]" after
// stripping the trailing `[N]` index. Multiple maps at different positions all share
// the leaf kind-name "map" — any match is sufficient since AWF5002 is a "this syntax
// is deferred" diagnostic, not a precise binding resolver.
//
// PRECEDENCE (HI-A, slice 3.4): callers MUST check producers[id] FIRST and only fall
// through to isMapID when id is NOT a known step producer. A workflow with both
// `step.id: "map"` AND a literal Map kind shares the leaf-name "map"; the step
// interpretation takes precedence so AWF5002 doesn't trip on a legit step ref. The
// corner case where a step AND a map share the name AND the step has `items` in its
// schema AND the author meant map aggregation silently mis-resolves (no diagnostic) —
// acceptable false-negative; aggregation is deferred anyway, so the author can't
// actually use the result until a later phase.
func isMapID(name string, mapIDs map[string]bool) bool {
	for mapPath := range mapIDs {
		// Take the LAST dotted segment (the leaf): e.g. "loop[0].body.map[1]" → "map[1]".
		leaf := mapPath
		if dot := strings.LastIndex(leaf, "."); dot >= 0 {
			leaf = leaf[dot+1:]
		}
		// Strip the trailing positional "[N]" suffix: "map[1]" → "map".
		if br := strings.Index(leaf, "["); br >= 0 {
			leaf = leaf[:br]
		}
		if leaf == name {
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
