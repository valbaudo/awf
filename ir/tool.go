package ir

// Tool is one entry of the top-level tools: map (P3 A4). The map KEY is the tool
// name (sent to the model); Tool is the value. A tool is offered to a react: step's
// model, which calls it by name.
type Tool struct {
	Description string      `json:"description"`
	InputSchema *JSONSchema `json:"input_schema,omitempty"`
	Impl        ToolImpl    `json:"impl"`
}

// ToolImpl is the executable body of a tool — a run: step that names a
// containers:-declared container. It is a DEDICATED id-less type, NOT a reused
// CodeStep: CodeStep.ID is `json:"id"` WITHOUT omitempty, so embedding a CodeStep
// would serialize an empty "id":"" into the JCS workflow digest. At execution time
// the engine synthesizes a real CodeStep from these fields (the reduce.go pattern).
// run:-ONLY — ir.CodeStep has no Exec field, so an exec: form cannot be synthesized
// into a CodeStep (rev #20). All fields are omitempty so an absent field never enters
// the digest.
type ToolImpl struct {
	Run         string            `json:"run,omitempty"`
	Container   string            `json:"container,omitempty"`
	Timeout     *Duration         `json:"timeout,omitempty"`
	OutputFiles OutputFiles       `json:"output_files,omitempty"`
	InputFiles  map[string]string `json:"input_files,omitempty"`
	Retry       *RetryPolicy      `json:"retry,omitempty"`
}
