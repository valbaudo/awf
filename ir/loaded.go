package ir

// LoadedDefinition is the loader's output and the validator's input — the parsed Workflow plus the
// raw bytes of every referenced compose file, keyed by the cleaned workflow-relative forward-slash
// path. That key form is the shape the spec's compose-fold consumes (see §E of the runtime
// design); the fold itself is not implemented yet — see ir/digest.go, which currently hashes only
// the canonical Workflow JSON. The fold lands when the CLI prints the canonical digest.
//
// Lives in the `ir` package because validation — also in `ir` per docs/runtime-design.md —
// consumes it. Putting LoadedDefinition in `loader` would create a cycle (loader already imports
// ir for Workflow/Container/Node).
type LoadedDefinition struct {
	// Workflow is the parsed IR. Non-nil if Load succeeded.
	Workflow *Workflow

	// WorkflowPath is the absolute path of the workflow file (for error attribution).
	WorkflowPath string

	// ComposeFiles maps each container's cleaned workflow-relative compose path
	// (e.g. "lab/compose.yml") to the raw bytes the loader read. Forward-slashed regardless
	// of OS so the digest input is portable.
	ComposeFiles map[string][]byte
}
