package engine

import (
	"github.com/valbaudo/awf/template"
)

// payloadScope is a template.Scope over a single signal's parsed JSON payload.
// It is the value source for the `signal.<field>` root in a keyed-await
// `where:` clause (F18): `signal.candidate_id` resolves to
// payload["candidate_id"], `signal.meta.id` descends. Callers reach it only
// through signalScope below, which strips the leading `signal` segment before
// delegating here — payloadScope itself never sees that segment.
//
// The AWF4001 inline-size check is applied by template.resolveRefValue AFTER
// Resolve returns (template/eval.go), so this scope must not duplicate it.
// Unknown roots/fields return AWF4002 (EvalCodeRefUnresolved), matching
// engine.Scope's convention. Reuses descendPath (engine/scope.go) for the walk.
type payloadScope struct {
	payload map[string]any
}

func newPayloadScope(payload map[string]any) *payloadScope {
	return &payloadScope{payload: payload}
}

// Resolve implements template.Scope. The first segment must be an identifier
// (a payload field name); subsequent segments descend via descendPath, which
// handles both object fields and array indices.
func (s *payloadScope) Resolve(ref *template.Ref) (any, error) {
	if len(ref.Segments) == 0 {
		return nil, &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: "empty ref"}
	}
	if err := mustIdent(ref.Segments[0], "where: payload field"); err != nil {
		return nil, err
	}
	if s.payload == nil {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved,
			"where: signal payload is not a JSON object (cannot resolve %q)", ref.Segments[0].Ident)
	}
	// descendPath walks from the payload map down every segment, returning
	// AWF4002 with a "payload." prefix on any missing field / type mismatch.
	return descendPath(s.payload, ref.Segments, "payload.")
}

// signalScope composes the engine's outer step scope with a candidate signal's
// payloadScope for a where: clause (F18). `signal.<field>` routes to the
// candidate payload; every other root (input.*, step.*, run.*, <as>.*, …)
// routes to outer — the exact same engine.Scope every other `{{ }}`
// expression in the workflow resolves against. "signal" is therefore a
// RESERVED ref root inside a where: clause: if an enclosing map's `as:`
// binding happens to be named "signal" it is shadowed within the where:
// clause specifically (outside where:, the binding still resolves normally).
//
// This is the ONLY scope-composition site in the engine — every other {{ }}
// field (run:, if.cond, loop.until, …) resolves against a single engine.Scope.
// where: needs two sources because signal.* varies per candidate payload
// while every other root is constant for the whole ReceiveMatching call.
type signalScope struct {
	payload *payloadScope
	outer   template.Scope
}

// Resolve implements template.Scope. root=="signal" strips that segment and
// delegates to payload; every other root delegates to outer unchanged.
func (s *signalScope) Resolve(ref *template.Ref) (any, error) {
	if isSignalRootRef(ref) {
		return s.payload.Resolve(&template.Ref{Segments: ref.Segments[1:]})
	}
	return s.outer.Resolve(ref)
}

// isSignalRootRef reports whether ref's first segment is the reserved `signal`
// root (routing it to the payload scope). Shared by signalScope.Resolve (routing)
// and buildWherePredicateWithScope's eager outer-ref pre-check (which SKIPS
// signal.* refs — they vary per payload and are resolved per-candidate).
func isSignalRootRef(ref *template.Ref) bool {
	return len(ref.Segments) > 0 && !ref.Segments[0].IsIndex && ref.Segments[0].Ident == "signal"
}
