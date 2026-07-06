package ir

import "encoding/json"

// wrap encodes a control node's inner value under a single wrapper key — `{"<keyword>": <inner>}`.
// Callers pass the node via a local alias type (e.g. `type a Gate; (*a)(n)`) so the alias's empty
// method set bypasses the node's MarshalJSON, avoiding infinite recursion while preserving fields
// and their NodeList tags.
func wrap(key string, inner any) ([]byte, error) {
	b, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{key: b})
}

// Six object-valued control nodes share the same wrap pattern. Map has its own
// dedicated MarshalJSON below (F51's two-arm `over:` union needs to fold OverItems
// into the same wire key as Over, which the generic one-liner can't express).
func (n *If) MarshalJSON() ([]byte, error)    { type a If; return wrap("if", (*a)(n)) }
func (n *Loop) MarshalJSON() ([]byte, error)  { type a Loop; return wrap("loop", (*a)(n)) }
func (n *Try) MarshalJSON() ([]byte, error)   { type a Try; return wrap("try", (*a)(n)) }
func (n *Gate) MarshalJSON() ([]byte, error)  { type a Gate; return wrap("gate", (*a)(n)) }
func (n *React) MarshalJSON() ([]byte, error) { type a React; return wrap("react", (*a)(n)) }
func (n *Compose) MarshalJSON() ([]byte, error) {
	type a Compose
	return wrap("compose", (*a)(n))
}

// MarshalJSON re-emits Map's "over" key as whichever arm of the F51 two-arm union is
// live: the literal sequence (OverItems) if non-nil, else the `{{ }}` expression string
// (Over) as before. OverItems itself is tagged json:"-" on the struct (node.go), so the
// embedded alias below never emits a second "over_items" key — only the explicit `Over
// json.RawMessage` field (which shadows the alias's own promoted "over" field) does.
//
// NOTE: putting the explicit "over" field before the embedded alias means the RAW (pre-JCS)
// byte output can order "over" ahead of the alias's "id" — NOT the struct's field order.
// This is DELIBERATELY accepted: every digest/round-trip path runs jcs.Transform (RFC 8785
// key-sort) downstream, so the raw order is never observed. The F51 expression-arm digest is
// pinned by TestDigestF51ExpressionArmGolden to catch any accidental value shift here.
func (n *Map) MarshalJSON() ([]byte, error) {
	type a Map
	var overSrc any = n.Over
	if n.OverItems != nil {
		overSrc = n.OverItems
	}
	overJSON, err := json.Marshal(overSrc)
	if err != nil {
		return nil, err
	}
	aux := struct {
		Over json.RawMessage `json:"over"`
		*a
	}{Over: overJSON, a: (*a)(n)}
	return wrap("map", aux)
}

// Parallel marshals to the standard's array-value form: {"parallel":[<node>,...]} (§5.4).
func (n *Parallel) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]NodeList{"parallel": n.Children})
}

// Skip marshals to the standard's string-value form: {"skip":"<reason>"} (§5.6).
func (n *Skip) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"skip": n.Reason})
}
