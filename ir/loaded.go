package ir

// LoadedDefinition is the loader's output and the validator's input — the parsed Workflow plus the
// raw bytes of every referenced compose file, keyed by the cleaned workflow-relative forward-slash
// path. That key form is what ir/digest.go consumes for the spec §E compose-fold (length-prefixed
// (path, sha256(bytes)) entries, sorted by path, folded into the workflow's content digest).
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
