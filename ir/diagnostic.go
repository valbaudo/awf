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
// bare iota int. The CLI's `--output json` output is the primary consumer; opaque integers
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
// catalog below; never renumbered (the catalog is an API for --output json consumers).
//
// Slice 1.4 collects all diagnostics in a single pass (no fail-fast). Slice 1.6's CLI computes
// the exit code via HasErrors.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Source   string   `json:"source,omitempty"`
	Path     string   `json:"path"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
}

// String renders a Diagnostic in human-readable form for the default CLI output. Format:
//
//	error AWF1005 at graph[0].run: image is a tag, not a digest
//
// The JSON projection (`awf validate --output json`) marshals the struct via its lowercase
// json tags (severity/source/path/code/message), so consumers like `jq '.diagnostics[].code'`
// read all-lowercase keys consistent with the validateResult envelope around them.
func (d Diagnostic) String() string {
	if d.Source != "" && d.Path != "" {
		return fmt.Sprintf("%s %s at %s:%s: %s", d.Severity, d.Code, d.Source, d.Path, d.Message)
	}
	if d.Source != "" {
		return fmt.Sprintf("%s %s at %s: %s", d.Severity, d.Code, d.Source, d.Message)
	}
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

// sortDiagnostics sorts ds in place by (Code, Source, Path, Message) so golden-file comparisons are
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
	if a.Source != b.Source {
		return a.Source < b.Source
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
	"AWF1012": "map missing a required field",
	"AWF1013": "gate.generate must be non-empty",
	"AWF1014": "gate.evaluate final node must declare output_schema",
	"AWF1015": "gate missing until",
	"AWF1016": "expression or template exceeds size limit",
	"AWF1017": "workflow `version` is not the supported value (only 1 is defined by AWF §2)",
	// AWF1018 is intentionally skipped (folded into AWF1009 after critique).
	"AWF1019": "container or service reference uses template syntax (`{{ }}`); these fields must be static names",
	"AWF1020": "step id has invalid characters or collides with a reserved addressing token",
	"AWF1021": "container snapshot value is not supported (only \"workspace\" or empty)",
	"AWF1022": "snapshot: workspace is only supported on image-mode containers, not compose",
	"AWF1023": "snapshot: workspace inside a map body is not yet supported (per-item snapshots land with the map slice); remove snapshot from this container or move it out of the map",
	"AWF1024": "env entry is not a valid environment-variable name",
	"AWF1025": "container is a map `image:` target and also declares a static image/compose; the static pin is silently overwritten per-element at dispatch — remove the static image/compose, or do not target it with a map `image:`",
	"AWF1026": "continues target is not an agent step (must be the id of another agent step, not code/signal/control)",
	"AWF1027": "continues target does not dominate this turn (must precede it in document order and every gate/map/loop/if-branch scope enclosing the target must also enclose this turn — forward/self refs, sibling branches, and gate/map/loop-internal targets are rejected)",
	"AWF1028": "continues links form a cycle",
	"AWF1029": "continues target must use the same agent runtime (uses) as this step",
	"AWF1030": "evaluator `continues:` may only target non-evaluator source context; evaluator transcript turns cannot be continued or included",
	"AWF1031": "continues target is unaddressable: it lies inside nested loops (a target's path may cross at most one loop)",
	"AWF1032": "continues target is a concurrent parallel sibling (not guaranteed to have run before this turn); continue a step outside the parallel instead",
	"AWF1033": "agents: role definition is invalid (missing uses:, or the role name collides with the <vendor>/<name> adapter-ref form)",
	"AWF1034": "uses references a name that is neither a declared agents: role nor a valid <vendor>/<name> adapter ref",
	"AWF1040": "skills: corpus declaration is invalid",
	"AWF1041": "skills: corpus references an unknown or non-directory asset",
	"AWF1042": "skills: corpus uses an unsupported layout or router",
	"AWF1043": "agent step skills: block is invalid",
	"AWF1044": "agent step skills.from references an unknown corpus",
	"AWF1045": "agent step skills: requires a container and non-overlapping staging destination",
	"AWF2001": "JSON Schema does not compile per the JSON Schema 2020-12 metaschema",
	"AWF2002": "agent output_schema violates §7 conservative cross-backend floor",
	"AWF2003": "containerless agent input_files per-file format/provider compatibility is checked at run time, not statically",
	"AWF3001": "reference to a step field that is not declared in the producer's output_schema",
	"AWF3002": "agent output_schema declared but no reference into it",
	"AWF3003": "compose file contains a non-digest image (§3: every image in a referenced compose file must be @sha256:-pinned)",
	"AWF3004": "compose file failed to parse",
	"AWF3005": "compose file uses `extends:` or `include:` directives; the validator refuses these (they would follow arbitrary disk paths and bypass loader confinement)",
	"AWF3006": "code step declares output_schema but its run script never writes $AWF_OUTPUT",
	"AWF3007": "input_files reference is not a prior in-scope step output_files artifact or declared workflow asset",
	"AWF3008": "runtime compose service does not exist in the generated compose project",
	"AWF3009": "output_files contract metadata is invalid",
	"AWF3010": "loaded skill corpus has invalid skill directory layout",
	"AWF3011": "loaded skill corpus contains an unsafe skill id or unsafe skill file path",
	"AWF3012": "top-level output binds a step inside a conditional scope; if that branch is not taken the output key is omitted (and validation fails if output_schema marks the field required)",
	"AWF3013": "string-typed reference substituted unquoted into a run:/idempotency_key shell host; an attacker-controlled value can inject shell commands (CWE-78) — wrap the slot in double quotes",
	"AWF3014": "output_artifact requires output_schema, is mutually exclusive with output_files, and its name must be a valid identifier (valid on any output_schema agent step, container-backed or containerless)",
	"AWF3015": "run:/reduce.run hardcodes the docker-only staging path /work/.awf; use $AWF_STAGING_ROOT (native's staging root is workdir-relative)",
	"AWF5001": "reference to `evaluate.<field>` outside a gate's generate or until",
	"AWF5002": "map output aggregation across nested or loop-multiplied maps is not yet defined",
	"AWF5003": "reference to a step inside a gate or map body from outside that scope — gate/map-internal steps resolve only within the same attempt/item (read a gate's product via evaluate.<field>)",
	"AWF5004": "map output aggregation reference is only usable as another map's `over:` (an aggregate is an array; templating renders scalars only)",
	"AWF5005": "exit_code/stdout are not defined on map aggregates (a map-internal step aggregates to []outputs or []field only)",
	"AWF1035": "reduce: must declare exactly one of run: or quorum: (quorum requires field:; a run: reducer requires container:)",
	"AWF1036": "where: use the `{{ signal.field == ... }}` envelope form (bare-identifier where: was removed)",
	"AWF1037": "prune must declare a `score` field name and exactly one of `keep`/`stop_when` (`keep` must be a positive integer, `stop_when` must be a non-empty bounded boolean expression)",
	"AWF1038": "compose block is invalid (requires static as/from/service/body, non-empty body, and a scoped handle that does not collide)",
	"AWF1039": "runtime map image target container may only be referenced by its owning map and that map's body",
	"AWF1046": "unknown or invalid call target",
	"AWF1047": "call input contract invalid",
	"AWF1048": "workflow outputs/export contract invalid",
	"AWF1049": "workflow artifact export invalid",
	"AWF1050": "workflow input_files contract invalid",
	"AWF1051": "call input_files binding invalid",
	// P3 A3/A4 — tools: + react: validation (AWF1052–AWF1058).
	"AWF1052": "react: tools list is empty (at least one tool name is required)",
	"AWF1053": "react: tool name is not declared in the top-level tools: map",
	"AWF1054": "react: max_turns must be a positive integer (0 means use the default of 8; negative values are rejected)",
	"AWF1055": "react: output_schema may not declare a property named stop_reason (engine-reserved field)",
	"AWF1056": "tool impl must name a containers:-declared container (container field is missing or undeclared)",
	"AWF1057": "react: requires uses: awf/llm (the only Containerless+Threaded adapter; CLI adapters cannot drive a tool loop)",
	"AWF1058": "react: structured_output: ollama_format is incompatible with the OpenAI tool-call protocol",
	"AWF1059": "container name uses an unsupported charset (must be a path-safe identifier)",
	"AWF1060": "cmd:/keepalive: is image-mode only; it has no meaning on a compose: container (a service's command lives in the Compose file)",
	"AWF1061": "a reserved step-level key is nested inside with: (it will be ignored by the engine); move it to the step level (sibling of with:)",
	"AWF1062": "unknown key (not part of the workflow or step schema; typo'd keys silently do nothing — remove or correct it)",
	"AWF1063": "duration must be a quoted string (e.g. \"300s\", \"5m\"); a bare integer is not accepted (it would be read as nanoseconds)",
	"AWF1064": "retry.recovery must be one of \"continue\", \"restart\", or unset; an unrecognized value would silently fall back to the per-adapter default",
	// AWF1065 is a run-start CLI capability guard (cli/backend_features.go,
	// checkContainerlessRunCapability), not a static ir.Validate rule — whether
	// a bare `run:` step is a problem depends on the resolved --backend, which
	// validate never sees. Reserved here (catalog membership + uniqueness
	// coverage via TestCatalogCodesAreUnique) so the code space stays
	// append-only and collision-free even though no c.errf call in ir/
	// emits it.
	"AWF1065": "a container-less `run:` step requires native execution; it is incompatible with `--backend docker` — declare a `container:` or run native",
	"AWF1066": "wire key renamed; see the diagnostic message for the old and new spelling",
	"AWF1067": "role config may reference {{ input.* }} only in model, system_prompt, and top-level string with: values; templates in nested positions or map keys are never substituted",
	// Loader-stage import diagnostics can be projected through ir.Diagnostic by the CLI.
	"AWF_IMPORT_CYCLE":          "workflow import graph contains a cycle",
	"AWF_IMPORT_DECODE":         "workflow failed to decode",
	"AWF_IMPORT_DEPTH":          "workflow import graph exceeds maximum depth",
	"AWF_IMPORT_ID_INVALID":     "workflow import id is invalid",
	"AWF_IMPORT_PATH_ABSOLUTE":  "workflow import path must be relative",
	"AWF_IMPORT_PATH_BACKSLASH": "workflow import path uses backslash separators",
	"AWF_IMPORT_PATH_ESCAPE":    "workflow import path escapes the workflow directory",
	"AWF_IMPORT_PATH_INVALID":   "workflow import path is invalid",
	"AWF_IMPORT_READ":           "workflow import failed to read",
	"AWF_IMPORT_SYMLINK":        "workflow import path resolves through a symlink",
	"AWF5006":                   "reduce quorum/over names a body output field that no branch declares, or min_success and reduce:{quorum} are both declared on the same node",
	"AWF5007":                   "reduce: a fan-in producer is nested under a loop or more than one gate; reduce collects only a single gate's accepted attempt, so this output would be silently dropped — flatten the body or remove the reduce:",
	"AWF5008":                   "prune.score must name a numeric field declared in the output_schema of the map body's last step",
	"AWF5009":                   "map id without reduce requires a final body code/agent/signal step with output_schema",
	"AWF5010":                   "map aggregate product may only be referenced outside its producing map",
	"AWF5011":                   "map aggregate product id is only supported for a single non-gate non-loop map",
}

// CatalogText returns the catalog's stable message text for a diagnostic
// code, or "" if code is not registered. catalog itself stays unexported (it
// is validate's internal composition table, keyed by ad hoc call sites across
// ir/*.go), but some codes — AWF1065 chief among them — are emitted OUTSIDE
// ir/ entirely (cli/backend_features.go's run-start capability guard; see the
// AWF1065 catalog comment above) and until now had no way to read the
// catalog's text without hand-duplicating it as a literal. CatalogText is
// that read-only accessor: a second call site composing the same message
// from the same code can no longer drift from the catalog's wording.
func CatalogText(code string) string {
	return catalog[code]
}
