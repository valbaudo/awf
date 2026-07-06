package ir

// Reduce is the fan-in clause on a Map (C2a). (Parallel is deferred — its
// bare-array wire-form has no slot for a sibling reduce: key; see SP2 Task 8.)
// Exactly one of Quorum or Run is set (validator AWF1035). The reduced output
// REPLACES the node's per-item array output (engine/scope.go aggregateMapOutputs
// prefers the node-path NodeResult when Reduce != nil).
//
//   - Quorum form: the node succeeds iff at least Quorum branches produced a
//     true Field. Quorum reuses Ratio (= json.Number) — an int count or a
//     (0,1] fraction — exactly like Map.MinSuccess, which it generalizes.
//   - Run form: an author shell reducer. The engine stages every branch's named
//     output_files artifact + a canonical-JSON manifest of all branch typed
//     outputs into Container via Backend.CopyTo (the SP1 artifact channel), then
//     runs Run and commits OutputSchema/OutputFiles at the node path.
//
// Field was named Over through v0.2.0 (json:"over"); renamed (F16) because it
// collides in spelling — but not in meaning — with Map.Over (the fan-out
// expression). The old `over:` spelling under reduce: is now a hard rename,
// detected position-aware by validateUnknownKeys (AWF1064): Map's own `over:`
// is unaffected since the renamed-key set is registered per Go struct type.
type Reduce struct {
	Quorum       *Ratio      `json:"quorum,omitempty"`
	Field        string      `json:"field,omitempty"`
	Run          string      `json:"run,omitempty"`
	Container    string      `json:"container,omitempty"`
	OutputSchema *JSONSchema `json:"output_schema,omitempty"`
	OutputFiles  OutputFiles `json:"output_files,omitempty"`
}

// IsQuorum / IsRun report which form is set. (Validator AWF1035 guarantees
// exactly one; these are the engine's branch.) Both are nil-receiver safe.
func (r *Reduce) IsQuorum() bool { return r != nil && r.Quorum != nil }
func (r *Reduce) IsRun() bool    { return r != nil && r.Run != "" }
