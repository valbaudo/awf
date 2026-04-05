package ir

import "fmt"

// Severity classifies a Diagnostic. Only Error contributes to the non-zero exit code that
// `awf validate` will return (slice 1.6); warnings inform but don't fail the run.
type Severity int

const (
	Error Severity = iota
	Warning
)

func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warning:
		return "warning"
	}
	return fmt.Sprintf("Severity(%d)", s)
}

// MarshalJSON emits the human-readable severity name ("error" / "warning") rather than the
// bare iota int. The CLI's `--format json` output is the primary consumer; opaque integers
// would be unusable for CI / IDE / dashboard tooling that reads the diagnostic stream.
func (s Severity) MarshalJSON() ([]byte, error) {
	switch s {
	case Error:
		return []byte(`"error"`), nil
	case Warning:
		return []byte(`"warning"`), nil
	}
	return nil, fmt.Errorf("ir: unknown Severity %d", s)
}

// UnmarshalJSON is the inverse of MarshalJSON; lets tooling round-trip the diagnostic stream
// without needing a parallel string-to-int mapping.
func (s *Severity) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case `"error"`:
		*s = Error
		return nil
	case `"warning"`:
		*s = Warning
		return nil
	}
	return fmt.Errorf("ir: unknown Severity %s", b)
}

// Diagnostic is one validation finding. Path is the static IR path (see ir/path.go) where the
// issue lives — empty for top-level / definition-wide findings. Code is a stable enum from the
// catalog below; never renumbered (the catalog is an API for --format json consumers).
//
// Slice 1.4 collects all diagnostics in a single pass (no fail-fast). Slice 1.6's CLI computes
// the exit code via HasErrors.
type Diagnostic struct {
	Severity Severity
	Path     string
	Code     string
	Message  string
}

// String renders a Diagnostic in human-readable form for the default CLI output. Format:
//
//	error AWF1005 at graph[0].run: image is a tag, not a digest
//
// The JSON projection (slice 1.6's `--format json`) marshals the struct directly with the
// default Go field names.
func (d Diagnostic) String() string {
	if d.Path == "" {
		return fmt.Sprintf("%s %s: %s", d.Severity, d.Code, d.Message)
	}
	return fmt.Sprintf("%s %s at %s: %s", d.Severity, d.Code, d.Path, d.Message)
}

// HasErrors reports whether ds contains at least one Diagnostic of Severity Error. Used by
// the CLI (slice 1.6) to decide the exit code: zero only when HasErrors returns false.
func HasErrors(ds []Diagnostic) bool {
	for _, d := range ds {
		if d.Severity == Error {
			return true
		}
	}
	return false
}

// sortDiagnostics sorts ds in place by (Code, Path, Message) so golden-file comparisons are
// stable. Uses a hand-rolled insertion sort to avoid importing "sort" for a single call
// site — diagnostic counts are tiny (≤ low hundreds even for a large workflow).
func sortDiagnostics(ds []Diagnostic) {
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && less(ds[j], ds[j-1]); j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
}

func less(a, b Diagnostic) bool {
	if a.Code != b.Code {
		return a.Code < b.Code
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	return a.Message < b.Message
}

// catalog is the source of truth for diagnostic codes. Keep entries sorted by code so the
// uniqueness test (TestCatalogCodesAreUnique) renders diffs cleanly. The empty placeholder
// here is filled in by Tasks 2–5 as each rule family lands; the entry MUST exist before its
// emission site references it (the validator references catalog[code] to compose the message
// template, so an unregistered code is a compile-time-detectable mistake).
//
// Reserved ranges (per Phase 1 design §C):
//
//	AWF1xxx structural — §4 (steps), §5 (control flow), §3 (containers)
//	AWF2xxx schema     — JSON Schema 2020-12 well-formedness + §7 floor
//	AWF3xxx digest/ref — output_schema-iff-referenced, compose digest-pinning
//
// AWF1001/AWF1002 are emitted at PARSE time by ir/node_unmarshal.go (zero / multiple
// kind-keys on a node) before Validate ever runs; they intentionally don't appear here.
var catalog = map[string]string{
	// Tasks 2–5 populate this. Task 1 leaves it empty so the uniqueness test passes
	// trivially and later additions are caught by code review (any new emission site
	// requires a corresponding catalog entry — that pairing is the convention).
}
