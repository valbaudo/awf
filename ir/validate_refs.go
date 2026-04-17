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
	indexProducers(wf.Graph, "", producers)

	// Synthetic "input" producer so the same checkRef machinery can validate input.<field>
	// against wf.Input. Path "input" mirrors what TestSchemaInputSchemaAlsoValidated uses.
	if wf.Input != nil {
		producers["input"] = producer{path: "input", kind: "input", schema: wf.Input}
	}

	// Track which producers had at least one ref into them (for AWF3002).
	referenced := map[string]bool{}

	// Walk the graph collecting refs from every Template and Expr field. evaluateAllowed=false
	// at the top level — only the gate frame's generate/until flip it true (see walkRefs).
	walkRefs(wf.Graph, "", c, producers, referenced, false)

	// AWF3002: any AgentStep with an output_schema but no inbound ref → warning.
	for id, p := range producers {
		if p.kind == "agent" && p.schema != nil && !referenced[id] {
			c.warnf(p.path, "AWF3002", fmt.Sprintf("%s (step %q)", catalog["AWF3002"], id))
		}
	}
}

func indexProducers(nodes NodeList, parent string, producers map[string]producer) {
	for i, n := range nodes {
		switch v := n.(type) {
		case *CodeStep:
			producers[v.ID] = producer{path: PathFor(parent, "", v.ID, i), kind: "code", schema: v.OutputSchema}
		case *AgentStep:
			producers[v.ID] = producer{path: PathFor(parent, "", v.ID, i), kind: "agent", schema: v.OutputSchema}
		case *SignalStep:
			producers[v.ID] = producer{path: PathFor(parent, "", v.ID, i), kind: "signal", schema: v.OutputSchema}
		case *If:
			indexProducers(v.Then, ChildPath(parent, "if", i, "then"), producers)
			indexProducers(v.Else, ChildPath(parent, "if", i, "else"), producers)
		case *Loop:
			indexProducers(v.Body, ChildPath(parent, "loop", i, "body"), producers)
		case *Try:
			indexProducers(v.Do, ChildPath(parent, "try", i, "do"), producers)
			indexProducers(v.Catch, ChildPath(parent, "try", i, "catch"), producers)
			indexProducers(v.Finally, ChildPath(parent, "try", i, "finally"), producers)
		case *Parallel:
			indexProducers(v.Children, PathFor(parent, "parallel", "", i), producers)
		case *Gate:
			indexProducers(v.Generate, ChildPath(parent, "gate", i, "generate"), producers)
			indexProducers(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), producers)
		case *Map:
			indexProducers(v.Body, ChildPath(parent, "map", i, "body"), producers)
		}
	}
}

// walkRefs visits every Template and Expr field in the graph and processes its refs.
//
// evaluateAllowed gates emission of AWF5001 in checkRef's `evaluate` case: true means
// `evaluate.<field>` is legal in this subtree, false means it errors. The bool propagates
// unchanged through every non-Gate node, so it represents the innermost gate frame's
// allow/deny — nested gates OVERRIDE (the inner frame's value is what walkRefs passes down).
func walkRefs(nodes NodeList, parent string, c *collector, producers map[string]producer, referenced map[string]bool, evaluateAllowed bool) {
	for i, n := range nodes {
		switch v := n.(type) {
		case *CodeStep:
			path := PathFor(parent, "", v.ID, i)
			checkTemplateRefs(v.Run, path+".run", c, producers, referenced, evaluateAllowed)
			if v.IdempotencyKey != nil {
				checkTemplateRefs(string(*v.IdempotencyKey), path+".idempotency_key", c, producers, referenced, evaluateAllowed)
			}
		case *AgentStep:
			path := PathFor(parent, "", v.ID, i)
			if v.IdempotencyKey != nil {
				checkTemplateRefs(string(*v.IdempotencyKey), path+".idempotency_key", c, producers, referenced, evaluateAllowed)
			}
			// v.With is opaque RawConfig per CLAUDE.md — do NOT walk it.
		case *SignalStep:
			// no Template / Expr fields beyond the schema itself.
		case *If:
			path := PathFor(parent, "if", "", i)
			checkExprRefs(string(v.Cond), path+".cond", c, producers, referenced, evaluateAllowed)
			walkRefs(v.Then, ChildPath(parent, "if", i, "then"), c, producers, referenced, evaluateAllowed)
			walkRefs(v.Else, ChildPath(parent, "if", i, "else"), c, producers, referenced, evaluateAllowed)
		case *Loop:
			path := PathFor(parent, "loop", "", i)
			if v.Until != nil {
				checkExprRefs(string(*v.Until), path+".until", c, producers, referenced, evaluateAllowed)
			}
			walkRefs(v.Body, ChildPath(parent, "loop", i, "body"), c, producers, referenced, evaluateAllowed)
		case *Try:
			walkRefs(v.Do, ChildPath(parent, "try", i, "do"), c, producers, referenced, evaluateAllowed)
			walkRefs(v.Catch, ChildPath(parent, "try", i, "catch"), c, producers, referenced, evaluateAllowed)
			walkRefs(v.Finally, ChildPath(parent, "try", i, "finally"), c, producers, referenced, evaluateAllowed)
		case *Parallel:
			walkRefs(v.Children, PathFor(parent, "parallel", "", i), c, producers, referenced, evaluateAllowed)
		case *Gate:
			path := PathFor(parent, "gate", "", i)
			// gate.until: evaluate.* allowed (single Expr field, no recursion).
			checkExprRefs(string(v.Until), path+".until", c, producers, referenced, true)
			// gate.generate: evaluate.* allowed (innermost frame OVERRIDES enclosing).
			walkRefs(v.Generate, ChildPath(parent, "gate", i, "generate"), c, producers, referenced, true)
			// gate.evaluate: evaluate.* REJECTED (the evaluator can't reference its own in-flight output).
			walkRefs(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), c, producers, referenced, false)
		case *Map:
			path := PathFor(parent, "map", "", i)
			checkExprRefs(string(v.Over), path+".over", c, producers, referenced, evaluateAllowed)
			// v.Container is a STATIC container name (AWF §5.7); validated by walkStructural
			// (AWF1009/AWF1019). Not a Template — no Slots/ParseRef walk here.
			walkRefs(v.Body, ChildPath(parent, "map", i, "body"), c, producers, referenced, evaluateAllowed)
		}
	}
}

// checkTemplateRefs scans src (an ir.Template field) for `{{ … }}` slots, parses each as a
// ref via the template package, and runs each through checkRef. evaluateAllowed is propagated
// to checkRef so the `evaluate.<field>` scope rule (AWF5001) can fire.
func checkTemplateRefs(src, path string, c *collector, producers map[string]producer, referenced map[string]bool, evaluateAllowed bool) {
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
		checkRef(*ref, path, c, producers, referenced, evaluateAllowed)
	}
}

// checkExprRefs unwraps the outer `{{ }}` envelope (if present), parses the inner as an
// Expr via the template package, and runs each Ref in the AST through checkRef. evaluateAllowed
// is propagated to checkRef so the `evaluate.<field>` scope rule (AWF5001) can fire.
func checkExprRefs(src, path string, c *collector, producers map[string]producer, referenced map[string]bool, evaluateAllowed bool) {
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
		checkRef(ref, path, c, producers, referenced, evaluateAllowed)
	}
}

// checkRef classifies a ref by its first segment and applies the appropriate cross-check.
// evaluateAllowed controls whether `evaluate.<field>` is legal in this position — false
// emits AWF5001 (the static counterpart of engine.Scope.resolveEvaluate's runtime check).
func checkRef(ref template.Ref, path string, c *collector, producers map[string]producer, referenced map[string]bool, evaluateAllowed bool) {
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
		p, ok := producers[id]
		if !ok {
			c.errf(path, "AWF3001", fmt.Sprintf("reference to undeclared step %q", id))
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
