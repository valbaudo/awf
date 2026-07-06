package ir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// controlKeys / stepKeys are the kind-key factory registries used by unmarshalNode for dispatch.
// They are the single source of truth for what counts as a node kind on the wire. See the
// touch-point note atop node.go before adding or removing an entry here.
var controlKeys = map[string]func() Node{
	"if": func() Node { return &If{} }, "loop": func() Node { return &Loop{} },
	"try": func() Node { return &Try{} }, "parallel": func() Node { return &Parallel{} },
	"gate": func() Node { return &Gate{} }, "skip": func() Node { return &Skip{} },
	"map": func() Node { return &Map{} }, "compose": func() Node { return &Compose{} },
	"react": func() Node { return &React{} },
}
var stepKeys = map[string]func() Node{
	"run": func() Node { return &CodeStep{} }, "uses": func() Node { return &AgentStep{} },
	"await": func() Node { return &SignalStep{} }, "call": func() Node { return &CallStep{} },
}

// unmarshalNode decodes one node by key-presence. Exactly one kind key must be present.
// Unknown extra keys are tolerated here; strict structural rejection lives in the validator —
// see runtime-design.md §4.
func unmarshalNode(raw json.RawMessage) (Node, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	var found []string
	for k := range controlKeys {
		if _, ok := probe[k]; ok {
			found = append(found, k)
		}
	}
	for k := range stepKeys {
		if _, ok := probe[k]; ok {
			found = append(found, k)
		}
	}
	// Sort so error messages and the picked single key are deterministic — Go map iteration is randomized.
	sort.Strings(found)
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("node has no kind key (need one of run/uses/await/call or a control keyword)")
	case 1:
		// ok
	default:
		return nil, fmt.Errorf("node has multiple kind keys: %v", found)
	}
	key := found[0]
	if mk, ok := controlKeys[key]; ok {
		n := mk()
		if err := unmarshalControl(key, probe[key], n); err != nil {
			return nil, err
		}
		return n, nil
	}
	n := stepKeys[key]()
	if err := json.Unmarshal(raw, n); err != nil { // step nodes are flat
		return nil, err
	}
	return n, nil
}

// unmarshalControl decodes the inner value of a control wrapper into n. For object-valued kinds
// the local alias type drops the node's method set while preserving its fields (and the NodeList
// fields' UnmarshalJSON, which recurses). Parallel and Skip diverge per the standard: their inner
// is an array and a string respectively, decoded directly into the relevant field.
func unmarshalControl(key string, inner json.RawMessage, n Node) error {
	switch v := n.(type) {
	case *If:
		type a If
		return json.Unmarshal(inner, (*a)(v))
	case *Loop:
		type a Loop
		return json.Unmarshal(inner, (*a)(v))
	case *Try:
		type a Try
		return json.Unmarshal(inner, (*a)(v))
	case *Parallel:
		// inner is the array itself (standard §5.4), not an object.
		return json.Unmarshal(inner, &v.Children)
	case *Gate:
		type a Gate
		return json.Unmarshal(inner, (*a)(v))
	case *Skip:
		// inner is the reason string (standard §5.6), not an object. `null` decodes cleanly
		// to "" (no error), giving the optional-reason form.
		return json.Unmarshal(inner, &v.Reason)
	case *Map:
		// NOT the alias-bypass pattern the other cases use: Map has a custom
		// UnmarshalJSON (F51's two-arm `over:` union, below) that must run, so this
		// dispatches straight to it instead of dropping to a method-less alias type.
		return v.UnmarshalJSON(inner)
	case *React:
		type a React
		return json.Unmarshal(inner, (*a)(v))
	case *Compose:
		type a Compose
		return json.Unmarshal(inner, (*a)(v))
	default:
		return fmt.Errorf("unknown control key %q", key)
	}
}

// UnmarshalJSON dispatches each raw element through unmarshalNode (key-presence union).
// NodeList is the type of every node-bearing field; see node.go.
func (ns *NodeList) UnmarshalJSON(b []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		return err
	}
	out := make([]Node, 0, len(raws))
	for _, r := range raws {
		n, err := unmarshalNode(r)
		if err != nil {
			return err
		}
		out = append(out, n)
	}
	*ns = out
	return nil
}

// UnmarshalJSON decodes a Map's inner object (b is unwrapped — the value under the "map"
// wire key, per unmarshalControl) and shape-dispatches its "over" field: F51's two-arm
// union. A JSON string decodes into Over (the `{{ }}` expression arm, exactly as before a
// literal sequence was accepted). A JSON array decodes into OverItems (the literal-sequence
// arm) after rejecting any item that contains a `{{ }} ` template — the literal arm is
// fully static, no engine-scope substitution is ever applied to it. An absent or null
// "over" key leaves both arms empty/nil — NOT a decode error — so a workflow missing
// `over:` entirely still decodes cleanly and AWF1012 (structural validation) reports the
// missing-field diagnostic exactly as it did before F51 (see
// testdata/invalid/AWF1012-map-missing-fields.yaml). Every other Map field
// (id/as/container/image/concurrency/min_success/body/reduce/prune) decodes through the
// embedded alias exactly as unmarshalControl's other cases do, so nothing is dropped.
func (n *Map) UnmarshalJSON(b []byte) error {
	type mapAlias Map
	aux := struct {
		Over json.RawMessage `json:"over"`
		*mapAlias
	}{mapAlias: (*mapAlias)(n)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	over := bytes.TrimSpace(aux.Over)
	switch {
	case len(over) == 0, string(over) == "null":
		// Key absent, or explicit `over: null` — leave both arms empty; AWF1012 catches it.
		n.Over = ""
		n.OverItems = nil
	case over[0] == '"':
		var s string
		if err := json.Unmarshal(over, &s); err != nil {
			return fmt.Errorf("map: over: %w", err)
		}
		n.Over = Expr(s)
		n.OverItems = nil
	case over[0] == '[':
		var items []any
		if err := json.Unmarshal(over, &items); err != nil {
			return fmt.Errorf("map: over: %w", err)
		}
		for i, item := range items {
			if hasTemplateLiteral(item) {
				return fmt.Errorf("map: over[%d]: a literal `over:` sequence item may not contain a {{ }} template (the literal arm is fully static — use a `{{ }}` expression for over: instead if you need substitution)", i)
			}
		}
		n.OverItems = items
		n.Over = ""
	default:
		return fmt.Errorf("map: over must be a `{{ }}` expression string or a literal sequence (JSON array), got %s", over)
	}
	return nil
}

// hasTemplateLiteral reports whether v (a value produced by decoding JSON into `any` —
// string/float64/bool/nil/[]any/map[string]any) contains a `{{ }}` template anywhere,
// recursively — a literal `over:` item may be a flat object or a nested array. Mirrors the
// `{{`/`}}` substring convention used elsewhere in this package (e.g. validate_structural.go,
// validate_refs.go) rather than a full template-grammar parse, since the literal arm's whole
// point is that no templating ever applies to it.
func hasTemplateLiteral(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, "{{") || strings.Contains(t, "}}")
	case []any:
		for _, e := range t {
			if hasTemplateLiteral(e) {
				return true
			}
		}
	case map[string]any:
		for _, e := range t {
			if hasTemplateLiteral(e) {
				return true
			}
		}
	}
	return false
}
