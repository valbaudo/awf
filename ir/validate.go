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
//   - agents     (AWF1033/4) — top-level agents: role-definition shape (non-empty
//     uses:, role name not in the <vendor>/<name> adapter-ref form) and every
//     uses: ref resolving to a declared role OR a syntactically-valid base ref
//   - skills     (AWF1040-45, AWF3010-11) — native skill corpus declarations,
//     loaded directory layout, and agent-step routing shape/staging constraints
//   - calls      (AWF1046-51) — imported workflow call targets, input contracts,
//     workflow outputs, and workflow artifact exports
//   - reduce     (AWF1035, AWF5006, AWF1009) — map reduce: fan-in shape (exactly one
//     of run:/quorum:; quorum needs over:; a run: reducer needs a resolvable
//     container:) and quorum/over aggregation scope (over: names a real body field;
//     min_success and reduce:{quorum} are mutually exclusive)
//   - prune      (AWF1037, AWF5008) — map prune: frontier shape (a `score` field +
//     exactly one of keep: top(<k>) / stop_when:) and score-field binding (score
//     names a numeric field in the body's last step's output_schema)
//   - refs       (AWF3001/2) — output_schema-iff-referenced cross-walk via the template
//     package (Slots → ParseRef per Template; ParseExpr → References per Expr)
//   - input_files (AWF3007) — every input_files value is a static step.<id>.files.<name>
//     ref naming a prior in-scope step's NAMED output_files artifact (dst absolute + clean)
//   - output_files (AWF3009) — named output_files contract metadata shape and schema_ref assets
//   - schema     (AWF2001/2) — JSON Schema well-formedness + §7 floor (warning, agents only)
//   - compose    (AWF3003/4/5) — compose-go/v2 parse + digest-pinning of every inner image
//
// Validate performs no filesystem I/O of its own — all reads happened in loader.Load before
// the LoadedDefinition was constructed. (compose-go/v2 is configured with SkipExtends,
// SkipInclude, and SkipResolveEnvironment so its transitive file-following directives don't
// reopen a file-read primitive.)
//
// Validate(nil) returns one Error diagnostic (AWF1003) so the slice-1.6 CLI can surface a
// nil LoadedDefinition gracefully rather than panicking.
func Validate(ld *LoadedDefinition) []Diagnostic {
	if ld == nil || ld.Workflow == nil {
		return []Diagnostic{{
			Severity: Error,
			Code:     "AWF1003",
			Message:  catalog["AWF1003"],
		}}
	}
	c := &collector{}
	for _, mod := range validationModules(ld) {
		if mod.Workflow == nil {
			continue
		}
		prevSource := c.source
		c.source = mod.Source
		modLD := loadedDefinitionForValidationModule(mod)
		validateStructural(modLD, c)
		validateAgents(modLD, c)
		validateSkills(modLD, c)
		validateContinues(modLD, c)
		validateReduce(modLD, c)
		validatePrune(modLD, c)
		validateCalls(ld, mod, c)
		validateRefsModule(ld, mod, c)
		validateInputFilesModule(ld, mod, c)
		validateOutputFiles(modLD, c)
		validateWorkflowExports(ld, mod, c)
		validateAwfOutputWrites(mod.Workflow.Graph, c)
		validateSchema(modLD, c)
		validateCompose(modLD, c)
		c.source = prevSource
	}
	return c.sorted()
}

type validationModule struct {
	ModuleID string
	Workflow *Workflow
	Assets   map[string]LoadedAsset
	Source   string

	WorkflowPath string
	ComposeFiles map[string][]byte
}

func validationModules(ld *LoadedDefinition) []validationModule {
	if ld == nil {
		return nil
	}
	out := []validationModule{}
	_ = ld.WalkModules(func(mod *LoadedModule) error {
		if mod == nil {
			return nil
		}
		source := ""
		if mod.ID != "" {
			source = mod.WorkflowPath
		}
		out = append(out, validationModule{
			ModuleID:     mod.ID,
			Workflow:     mod.Workflow,
			Assets:       mod.Assets,
			Source:       source,
			WorkflowPath: mod.WorkflowPath,
			ComposeFiles: mod.ComposeFiles,
		})
		return nil
	})
	if len(out) == 0 && ld.Workflow != nil {
		out = append(out, validationModule{
			ModuleID:     "",
			Workflow:     ld.Workflow,
			Assets:       ld.Assets,
			WorkflowPath: ld.WorkflowPath,
			ComposeFiles: ld.ComposeFiles,
		})
	}
	return out
}

func loadedDefinitionForValidationModule(mod validationModule) *LoadedDefinition {
	return &LoadedDefinition{
		Workflow:     mod.Workflow,
		WorkflowPath: mod.WorkflowPath,
		ComposeFiles: mod.ComposeFiles,
		Assets:       mod.Assets,
	}
}

// collector accumulates Diagnostics across passes and produces a deterministically-ordered
// output. Used by every validate_*.go pass.
type collector struct {
	source string
	out    []Diagnostic
}

// errf appends an Error diagnostic at the given path with the given code. msg is the
// fully-formatted message — callers pre-format with fmt.Sprintf if interpolation is needed
// (the message is included verbatim in the rendered diagnostic and in --format json).
func (c *collector) errf(path, code, msg string) {
	c.out = append(c.out, Diagnostic{Severity: Error, Source: c.source, Path: path, Code: code, Message: msg})
}

// warnf appends a Warning diagnostic. See errf doc for the message-formatting contract.
func (c *collector) warnf(path, code, msg string) {
	c.out = append(c.out, Diagnostic{Severity: Warning, Source: c.source, Path: path, Code: code, Message: msg})
}

// sorted returns the collected diagnostics in a stable, deterministic order (by Code, then
// Source, then Path, then Message) so golden-file comparisons don't depend on map-iteration order
// or pass invocation order. Always returns a non-nil slice (an empty []Diagnostic{} on the clean path)
// so JSON consumers see "diagnostics": [] rather than null — important for downstream tooling
// like `jq '.diagnostics[]'` that breaks on null but works on [].
func (c *collector) sorted() []Diagnostic {
	out := make([]Diagnostic, 0, len(c.out))
	out = append(out, c.out...)
	sortDiagnostics(out)
	return out
}
