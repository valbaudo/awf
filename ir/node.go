package ir

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Node is one of the ten node kinds. Discriminated by key-presence (the AWF standard's surface).
type Node interface{ isNode() }

// --- Step nodes (flat objects discriminated by run/uses/await) ---

type CodeStep struct {
	ID             string       `json:"id"`
	Container      string       `json:"container,omitempty"`
	Run            string       `json:"run"`
	Timeout        *Duration    `json:"timeout,omitempty"`
	OutputSchema   *JSONSchema  `json:"output_schema,omitempty"`
	OutputFiles    []string     `json:"output_files,omitempty"`
	IdempotencyKey *Template    `json:"idempotency_key,omitempty"`
	Retry          *RetryPolicy `json:"retry,omitempty"`
}

type AgentStep struct {
	ID             string       `json:"id"`
	Container      string       `json:"container,omitempty"`
	Uses           string       `json:"uses"`
	With           RawConfig    `json:"with,omitempty"`
	OutputSchema   *JSONSchema  `json:"output_schema,omitempty"`
	OutputFiles    []string     `json:"output_files,omitempty"`
	Timeout        *Duration    `json:"timeout,omitempty"`
	IdempotencyKey *Template    `json:"idempotency_key,omitempty"`
	Retry          *RetryPolicy `json:"retry,omitempty"`
}

type SignalStep struct {
	ID           string      `json:"id"`
	Await        string      `json:"await"`
	Timeout      *Duration   `json:"timeout,omitempty"`
	OutputSchema *JSONSchema `json:"output_schema,omitempty"`
}

func (*CodeStep) isNode()   {}
func (*AgentStep) isNode()  {}
func (*SignalStep) isNode() {}

// --- Control nodes (single-key wrapper objects). Node-bearing fields are NodeList (a bare
// []Node interface slice cannot be unmarshaled — verified during planning). ---

type If struct {
	Cond Expr     `json:"cond"`
	Then NodeList `json:"then"`
	Else NodeList `json:"else,omitempty"`
}
type Loop struct {
	Until    *Expr    `json:"until,omitempty"`
	MaxIters *int     `json:"max_iters,omitempty"`
	Body     NodeList `json:"body"`
}
type Try struct {
	Do      NodeList `json:"do"`
	Catch   NodeList `json:"catch,omitempty"`
	Finally NodeList `json:"finally,omitempty"`
}
type Parallel struct {
	// Children is carried by Parallel's custom (un)marshalers as the wrapper's array value
	// (`{"parallel":[<node>,...]}` per the standard §5.4). json:"-" documents that fact and
	// keeps the tag-reflection test happy.
	Children NodeList `json:"-"`
}
type Gate struct {
	Generate    NodeList `json:"generate"`
	Evaluate    NodeList `json:"evaluate"`
	Until       Expr     `json:"until"`
	MaxAttempts int      `json:"max_attempts"`
}
type Skip struct {
	// Reason is carried by Skip's custom (un)marshalers as the wrapper's string value
	// (`{"skip":"<reason>"}` per the standard §5.6). json:"-" documents that fact and
	// keeps the tag-reflection test happy.
	Reason string `json:"-"`
}
type Map struct {
	Over        Expr     `json:"over"`
	As          string   `json:"as"`
	Container   string   `json:"container"`
	Concurrency int      `json:"concurrency"`
	MinSuccess  *Ratio   `json:"min_success,omitempty"`
	Body        NodeList `json:"body"`
}

func (*If) isNode()       {}
func (*Loop) isNode()     {}
func (*Try) isNode()      {}
func (*Parallel) isNode() {}
func (*Gate) isNode()     {}
func (*Skip) isNode()     {}
func (*Map) isNode()      {}

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

func (n *If) MarshalJSON() ([]byte, error)   { type a If; return wrap("if", (*a)(n)) }
func (n *Loop) MarshalJSON() ([]byte, error) { type a Loop; return wrap("loop", (*a)(n)) }
func (n *Try) MarshalJSON() ([]byte, error)  { type a Try; return wrap("try", (*a)(n)) }

// Parallel marshals to the standard's array-value form: {"parallel":[<node>,...]}.
func (n *Parallel) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]NodeList{"parallel": n.Children})
}
func (n *Gate) MarshalJSON() ([]byte, error) { type a Gate; return wrap("gate", (*a)(n)) }

// Skip marshals to the standard's string-value form: {"skip":"<reason>"}.
func (n *Skip) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"skip": n.Reason})
}
func (n *Map) MarshalJSON() ([]byte, error) { type a Map; return wrap("map", (*a)(n)) }

var controlKeys = map[string]func() Node{
	"if": func() Node { return &If{} }, "loop": func() Node { return &Loop{} },
	"try": func() Node { return &Try{} }, "parallel": func() Node { return &Parallel{} },
	"gate": func() Node { return &Gate{} }, "skip": func() Node { return &Skip{} },
	"map": func() Node { return &Map{} },
}
var stepKeys = map[string]func() Node{
	"run": func() Node { return &CodeStep{} }, "uses": func() Node { return &AgentStep{} },
	"await": func() Node { return &SignalStep{} },
}

// unmarshalNode decodes one node by key-presence. Exactly one kind key must be present.
// (Unknown extra keys are tolerated here; strict structural rejection is slice 1.4's job.)
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
	sort.Strings(found)
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("node has no kind key (need one of run/uses/await or a control keyword)")
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

// unmarshalControl decodes the inner object of a control wrapper into n. The local alias type drops
// any node-level method set while preserving the NodeList fields, whose UnmarshalJSON recurses.
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
	default:
		return fmt.Errorf("unknown control key %q", key)
	}
}

// NodeList is the type of every node-bearing field. A bare []Node cannot unmarshal (interface),
// so all such fields use this named type whose UnmarshalJSON dispatches each element by key-presence.
// Marshaling works through the elements' own MarshalJSON, so no custom marshaler is needed here.
type NodeList []Node

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
