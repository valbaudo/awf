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

// Five object-valued control nodes share the same wrap pattern.
func (n *If) MarshalJSON() ([]byte, error)   { type a If; return wrap("if", (*a)(n)) }
func (n *Loop) MarshalJSON() ([]byte, error) { type a Loop; return wrap("loop", (*a)(n)) }
func (n *Try) MarshalJSON() ([]byte, error)  { type a Try; return wrap("try", (*a)(n)) }
func (n *Gate) MarshalJSON() ([]byte, error) { type a Gate; return wrap("gate", (*a)(n)) }
func (n *Map) MarshalJSON() ([]byte, error)  { type a Map; return wrap("map", (*a)(n)) }

// Parallel marshals to the standard's array-value form: {"parallel":[<node>,...]} (§5.4).
func (n *Parallel) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]NodeList{"parallel": n.Children})
}

// Skip marshals to the standard's string-value form: {"skip":"<reason>"} (§5.6).
func (n *Skip) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"skip": n.Reason})
}
