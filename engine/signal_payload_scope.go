package engine

import (
	"github.com/valbaudo/awf/template"
)

// payloadScope is a template.Scope over a single signal's parsed JSON payload.
// It is the value source for the BARE identifiers in a keyed-await `where:`
// clause: `candidate_id` resolves to payload["candidate_id"], `meta.id` descends.
// `{{ … }}` slots are NOT resolved here — they are pre-substituted against the
// ENGINE scope before the expression reaches the evaluator (see runSignalStep).
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
