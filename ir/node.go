package ir

// Node is one of the thirteen node kinds (4 step + 9 control). Discriminated by key-presence at the
// wire level (the AWF standard's surface), so adding/removing a kind touches FOUR places in this
// package — keep them in sync:
//
//  1. The type declaration and isNode() method in this file (node.go).
//  2. The MarshalJSON method in node_marshal.go (either via `wrap` for object-valued kinds, or
//     a custom shape for Skip / Parallel per the standard).
//  3. The factory entry in `controlKeys` or `stepKeys` in node_unmarshal.go.
//  4. The case in `unmarshalControl` in node_unmarshal.go (control kinds only; step kinds use
//     a flat `json.Unmarshal` into the value the factory returned).
//
// TestNodeRegistryExhaustive guards (1)/(3)/(4); the tag-reflection test (tags_test.go) guards
// json tags on every exported field. Adding a node kind without all four edits surfaces as
// a test failure (or, in the worst case, as the "unknown control key" runtime error).
type Node interface{ isNode() }

// --- Step nodes (flat objects discriminated by run/uses/await/call) ---

type CodeStep struct {
	ID           string      `json:"id"`
	Container    string      `json:"container,omitempty"`
	Run          string      `json:"run"`
	Timeout      *Duration   `json:"timeout,omitempty"`
	OutputSchema *JSONSchema `json:"output_schema,omitempty"`
	OutputFiles  OutputFiles `json:"output_files,omitempty"`
	// InputFiles maps an in-container destination path → a static artifact or asset
	// reference (step.<id>.files.<name> or asset.<id>). The engine resolves each to
	// a CAS blob from a prior named output_files commit or run-start asset snapshot
	// and stages the bytes via Backend.CopyTo BEFORE this step runs. Static, not a
	// {{ }} template (AWF3007). Requires a container (rejected on containerless
	// agent steps at runtime).
	InputFiles     map[string]string `json:"input_files,omitempty"`
	IdempotencyKey *Template         `json:"idempotency_key,omitempty"`
	Retry          *RetryPolicy      `json:"retry,omitempty"`
}

type AgentStep struct {
	ID           string      `json:"id"`
	Container    string      `json:"container,omitempty"`
	Uses         string      `json:"uses"`
	With         RawConfig   `json:"with,omitempty"`
	Continues    string      `json:"continues,omitempty"` // id of the prior agent turn to continue (engine-owned thread)
	OutputSchema *JSONSchema `json:"output_schema,omitempty"`
	OutputFiles  OutputFiles `json:"output_files,omitempty"`
	// OutputArtifact, valid ONLY on a containerless agent step, serializes the
	// step's validated typed Output as canonical JSON and publishes it as the
	// named artifact step.<id>.files.<name>. Mutually exclusive with
	// output_files; requires output_schema. The dispatcher produces the bytes
	// (a containerless step has no filesystem to capture from).
	OutputArtifact string            `json:"output_artifact,omitempty"`
	Skills         *StepSkillRouting `json:"skills,omitempty"`
	// InputFiles maps an in-container destination path → a static artifact or asset
	// reference (step.<id>.files.<name> or asset.<id>). The engine resolves each to
	// a CAS blob from a prior named output_files commit or run-start asset snapshot
	// and stages the bytes via Backend.CopyTo BEFORE this step runs. Static, not a
	// {{ }} template (AWF3007). Requires a container (rejected on containerless
	// agent steps at runtime).
	InputFiles     map[string]string `json:"input_files,omitempty"`
	Timeout        *Duration         `json:"timeout,omitempty"`
	IdempotencyKey *Template         `json:"idempotency_key,omitempty"`
	Retry          *RetryPolicy      `json:"retry,omitempty"`
}

type SignalStep struct {
	ID    string `json:"id"`
	Await string `json:"await"`
	// Where is an optional bounded-boolean clause selecting WHICH buffered signal
	// of name Await to consume. `{{ … }}` slots substitute from the surrounding
	// engine scope (e.g. {{ hyp.id }}); bare identifiers resolve against the
	// delivered signal's JSON payload (e.g. candidate_id == payload.candidate_id).
	// Empty → earliest-first per name (unchanged). Folds into the digest (omitempty
	// keeps where-less workflows byte-identical). Reuses the bounded boolean
	// evaluator — no arithmetic (spec §3.5 / §8 C5).
	Where        string      `json:"where,omitempty"`
	Timeout      *Duration   `json:"timeout,omitempty"`
	OutputSchema *JSONSchema `json:"output_schema,omitempty"`
}

type CallStep struct {
	ID         string                   `json:"id"`
	Call       string                   `json:"call"`
	Input      map[string]TemplateValue `json:"input,omitempty"`
	InputFiles map[string]string        `json:"input_files,omitempty"`
}

func (*CodeStep) isNode()   {}
func (*AgentStep) isNode()  {}
func (*SignalStep) isNode() {}
func (*CallStep) isNode()   {}

// --- Control nodes (single-key wrapper objects per the standard's YAML surface). Node-bearing
// fields use the NodeList named type — a bare []Node interface slice cannot be unmarshaled. ---

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
	ID          string   `json:"id,omitempty"`
	Over        Expr     `json:"over"`
	As          string   `json:"as"`
	Container   string   `json:"container"`
	Image       Template `json:"image,omitempty"`
	Concurrency int      `json:"concurrency"`
	MinSuccess  *Ratio   `json:"min_success,omitempty"`
	Body        NodeList `json:"body"`
	Reduce      *Reduce  `json:"reduce,omitempty"`
	// Prune is the optional frontier clause (SP5, spec §3.2b). nil for a plain
	// result-blind map. Folds into the digest via omitempty.
	Prune *Prune `json:"prune,omitempty"`
}
type Compose struct {
	As      string   `json:"as"`
	From    string   `json:"from"`
	Service Template `json:"service"`
	Body    NodeList `json:"body"`
}

// React is the model+tools+loop node (P3 A3). A control-style wrapper of the Map
// class: its runtime path is react[N] (keyword[index], NOT its id); the id is for
// output addressing only ({{ <id>.* }} / awf outputs --step <id>) via producer
// registration. tools is required and non-empty; with is the awf/llm config minus
// prompt (the engine owns the messages array). See spec §3.2.
type React struct {
	ID           string      `json:"id,omitempty"`
	With         RawConfig   `json:"with,omitempty"`
	Prompt       string      `json:"prompt"`
	Tools        []string    `json:"tools"`
	MaxTurns     int         `json:"max_turns,omitempty"`
	OutputSchema *JSONSchema `json:"output_schema,omitempty"`
}

func (*If) isNode()       {}
func (*Loop) isNode()     {}
func (*Try) isNode()      {}
func (*Parallel) isNode() {}
func (*Gate) isNode()     {}
func (*Skip) isNode()     {}
func (*Map) isNode()      {}
func (*Compose) isNode()  {}
func (*React) isNode()    {}

// NodeList is the type of every node-bearing field. A bare []Node cannot unmarshal (interface
// elements), so all such fields use this named type whose UnmarshalJSON (in node_unmarshal.go)
// dispatches each element by key-presence. Marshaling works through each element's own
// MarshalJSON, so no custom marshaler is needed for the slice itself.
type NodeList []Node
