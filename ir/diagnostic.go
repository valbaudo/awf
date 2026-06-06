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
// Reserved ranges (per Phase 1 design §C, extended in Phase 3 slice 3.3):
//
//	AWF1xxx structural — §4 (steps), §5 (control flow), §3 (containers)
//	AWF2xxx schema     — JSON Schema 2020-12 well-formedness + §7 floor
//	AWF3xxx digest/ref — output_schema-iff-referenced, compose digest-pinning
//	AWF5xxx control-flow scope — §5 (gate-scope evaluate.* rule, map
//	                            aggregation rules)
//
// AWF1001/AWF1002 are emitted at PARSE time by ir/node_unmarshal.go (zero / multiple
// kind-keys on a node) before Validate ever runs; they intentionally don't appear here.
var catalog = map[string]string{
	"AWF1003": "nil or empty LoadedDefinition",
	"AWF1004": "step id is not unique",
	"AWF1005": "container declares both image and compose (pick one)",
	"AWF1006": "container declares neither image nor compose",
	"AWF1007": "image must be content-addressed (tag forbidden, use @sha256:...)",
	"AWF1008": "compose-backed container missing required `service` field",
	"AWF1009": "container reference is missing or does not resolve to a declared container",
	"AWF1010": "parallel branches use overlapping containers (§5.4 forbids)",
	"AWF1011": "loop declares neither until nor max_iters",
	"AWF1012": "map missing one of over/as/container/concurrency",
	"AWF1013": "gate.generate must be non-empty",
	"AWF1014": "gate.evaluate final node must declare output_schema",
	"AWF1015": "gate missing until",
	"AWF1016": "expression or template exceeds size limit (default 64 KiB)",
	"AWF1017": "workflow `version` is not the supported value (only 1 is defined by AWF §2)",
	// AWF1018 is intentionally skipped (folded into AWF1009 after critique).
	"AWF1019": "container or service reference uses template syntax (`{{ }}`); these fields must be static names",
	"AWF1020": "step id has invalid characters or collides with a reserved addressing token",
	"AWF1021": "container snapshot value is not supported (only \"workspace\" or empty)",
	"AWF1022": "snapshot: workspace is only supported on image-mode containers, not compose",
	"AWF1023": "snapshot: workspace inside a map body is not yet supported (per-item snapshots land with the map slice); remove snapshot from this container or move it out of the map",
	"AWF1024": "env entry is not a valid environment-variable name (must match [A-Za-z_][A-Za-z0-9_]*)",
	"AWF1025": "container is a map `image:` target and also declares a static image/compose; the static pin is silently overwritten per-element at dispatch — remove the static image/compose, or do not target it with a map `image:`",
	"AWF1026": "continues target is not an agent step (must be the id of another agent step, not code/signal/control)",
	"AWF1027": "continues target does not dominate this turn (must precede it in document order and every gate/map/loop/if-branch scope enclosing the target must also enclose this turn — forward/self refs, sibling branches, and gate/map/loop-internal targets are rejected)",
	"AWF1028": "continues links form a cycle",
	"AWF1029": "continues target must use the same agent runtime (uses) as this step",
	"AWF1030": "a step inside a gate's evaluate: block may not use continues (the evaluator must judge in a fresh context)",
	"AWF1031": "continues target is unaddressable: it lies inside nested loops (a target's path may cross at most one loop)",
	"AWF1032": "continues target is a concurrent parallel sibling (not guaranteed to have run before this turn); continue a step outside the parallel instead",
	"AWF1033": "agents: role definition is invalid (missing uses:, or the role name collides with the <vendor>/<name> adapter-ref form)",
	"AWF1034": "uses references a name that is neither a declared agents: role nor a valid <vendor>/<name> adapter ref",
	"AWF2001": "JSON Schema does not compile per the JSON Schema 2020-12 metaschema",
	"AWF2002": "agent output_schema violates §7 conservative cross-backend floor",
	"AWF3001": "reference to a step field that is not declared in the producer's output_schema",
	"AWF3002": "agent output_schema declared but no reference into it",
	"AWF3003": "compose file contains a non-digest image (§3: every image in a referenced compose file must be @sha256:-pinned)",
	"AWF3004": "compose file failed to parse",
	"AWF3005": "compose file uses `extends:` or `include:` directives; the validator refuses these (they would follow arbitrary disk paths and bypass loader confinement)",
	"AWF3006": "code step declares output_schema but its run script never writes $AWF_OUTPUT",
	"AWF3007": "input_files reference is not a prior in-scope step's named output_files artifact (step.<id>.files.<name>)",
	"AWF5001": "reference to `evaluate.<field>` outside a gate's generate or until",
	"AWF5002": "map output aggregation across nested or loop-multiplied maps is not yet defined",
	"AWF5003": "reference to a step inside a gate or map body from outside that scope — gate/map-internal steps resolve only within the same attempt/item (read a gate's product via evaluate.<field>)",
	"AWF5004": "map output aggregation reference is only usable as another map's `over:` (an aggregate is an array; templating renders scalars only)",
	"AWF5005": "exit_code/stdout are not defined on map aggregates (a map-internal step aggregates to []outputs or []field only)",
	"AWF1035": "reduce: must declare exactly one of run: or quorum: (quorum requires over:; a run: reducer requires container:)",
	"AWF5006": "reduce quorum/over names a body output field that no branch declares, or min_success and reduce:{quorum} are both declared on the same node",
}
