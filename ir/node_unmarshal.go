package ir

import (
	"encoding/json"
	"fmt"
	"sort"
)

// controlKeys / stepKeys are the kind-key factory registries used by unmarshalNode for dispatch.
// They are the single source of truth for what counts as a node kind on the wire. See the
// touch-point note atop node.go before adding or removing an entry here.
var controlKeys = map[string]func() Node{
	"if": func() Node { return &If{} }, "loop": func() Node { return &Loop{} },
	"try": func() Node { return &Try{} }, "parallel": func() Node { return &Parallel{} },
	"gate": func() Node { return &Gate{} }, "skip": func() Node { return &Skip{} },
	"map": func() Node { return &Map{} }, "compose": func() Node { return &Compose{} },
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
		type a Map
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
