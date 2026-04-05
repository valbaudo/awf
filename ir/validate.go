package ir

// Validate is the AWF validator entry point. It walks ld in five passes (one per rule family)
// and returns every Diagnostic found — collect-all, no fail-fast. The returned slice is
// stable-sorted by (Code, Path, Message) so golden-file tests over the diagnostic stream are
// stable regardless of map-iteration order.
//
// Passes (each lives in its own validate_*.go file):
//
//   - structural (AWF1xxx)  — §4 step shape, §5 control-flow field requirements, §3
//     container shape, parallel/map distinct-container rule
//   - refs       (AWF3001/2) — output_schema-iff-referenced cross-walk via the template
//     package (Slots → ParseRef per Template; ParseExpr → References per Expr)
//   - schema     (AWF2001/2) — JSON Schema well-formedness + §7 floor (warning, agents only)
//   - compose    (AWF3003/4/5) — compose-go/v2 parse + digest-pinning of every inner image
//
// Validate is pure: no I/O, no clock, no goroutines. ld.WorkflowPath is read only for the
// `at <path>` suffix of compose diagnostics; the function never touches the filesystem.
//
// Validate(nil) returns one Error diagnostic (AWF1003) so the slice-1.6 CLI can surface a
// nil LoadedDefinition gracefully rather than panicking.
func Validate(ld *LoadedDefinition) []Diagnostic {
	if ld == nil || ld.Workflow == nil {
		return []Diagnostic{{
			Severity: Error,
			Code:     "AWF1003",
			Message:  "nil or empty LoadedDefinition",
		}}
	}
	c := &collector{}
	// Each pass lands in Tasks 2–5. The skeleton calls nothing — Task 2 will replace this
	// comment with the structural pass invocation, Task 3 with refs, etc.
	_ = c
	return c.sorted()
}

// collector accumulates Diagnostics across passes and produces a deterministically-ordered
// output. Used by every validate_*.go pass.
type collector struct {
	out []Diagnostic
}

// errf appends an Error diagnostic at the given path with the given code. msg is the
// fully-formatted message — callers pre-format with fmt.Sprintf if interpolation is needed
// (the message is included verbatim in the rendered diagnostic and in --format json).
func (c *collector) errf(path, code, msg string) {
	c.out = append(c.out, Diagnostic{Severity: Error, Path: path, Code: code, Message: msg})
}

// warnf appends a Warning diagnostic. See errf doc for the message-formatting contract.
func (c *collector) warnf(path, code, msg string) {
	c.out = append(c.out, Diagnostic{Severity: Warning, Path: path, Code: code, Message: msg})
}

// sorted returns the collected diagnostics in a stable, deterministic order (by Code, then
// Path, then Message) so golden-file comparisons don't depend on map-iteration order or pass
// invocation order.
func (c *collector) sorted() []Diagnostic {
	out := append([]Diagnostic(nil), c.out...)
	sortDiagnostics(out)
	return out
}
