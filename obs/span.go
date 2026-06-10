package obs

import "time"

// SpanStatus mirrors OTel's status codes without importing the SDK into the
// pure projection layer (Export translates it).
type SpanStatus int

const (
	StatusUnset SpanStatus = iota
	StatusOK
	StatusError
)

// SpanEvent is a point-in-time event on a span (e.g. gen_ai.evaluation.result).
type SpanEvent struct {
	Name       string
	Time       time.Time
	Attributes map[string]any
}

// Span is one node in the projected trace tree — the deterministic output of
// Project. It is plain data (no OTel SDK types) so Project(log) is byte-stable
// across replays (spec D6) and Export translates it to OTel at the edge.
//
// Path is the awf.node.path (unique per run); ParentPath is "" for the run
// root. Attribute values are restricted to string / int64 / float64 / bool so
// Export can map each to an attribute.KeyValue. A Pending span (started, never
// finalized) has Pending=true, End set to the log's last-observed event time
// (deterministic "as-of" bound), and Attributes["awf.node.outcome"]="incomplete".
//
// Scope distinguishes a real node (a leaf step built from node.started/
// completed/failed — Scope=false, carries awf.node.kind ∈ the 12 real kinds)
// from a synthesized control-scope (Scope=true, carries awf.scope.kind). It is
// the single discriminant for the leaf/scope tests in the bounds fold and the
// cost rollup — decoupled from the Kind string so structural roles can grow
// without breaking those (M1). Span is CLEAN public data: no projection
// bookkeeping fields leak into it (start-seen / finalized tracking lives in
// Project's local maps, not here).
type Span struct {
	Path       string
	ParentPath string
	Name       string
	Kind       string
	Scope      bool // true ⇒ synthesized control-scope; false ⇒ leaf step node
	Start      time.Time
	End        time.Time
	Pending    bool
	Status     SpanStatus
	StatusMsg  string
	Attributes map[string]any
	Events     []SpanEvent
}
