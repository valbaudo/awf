package ir

import "testing"

// Tests for the reduce-aware half of input_files validation (SP2 Task 11). When the
// producer's enclosing map declares a reduce:, an input_files
// step.<bodyId>.files.<name> ref resolves against the REDUCER's output_files (a
// run: reducer's Reduce.OutputFiles), NOT the body step's output_files — mirroring
// engine/scope.go ResolveArtifactPath's reduce branch (LookupCompleted(mapStatic)).
// A quorum reducer has no artifacts → .files.<name> still errors.

// reduceBodyScan is a map-body code step declaring a NAMED output_files artifact
// "leaf" (a per-item artifact that does NOT survive into the reducer's output).
func reduceBodyScan() *CodeStep {
	return &CodeStep{ID: "scan", Container: "c", Run: "true",
		OutputFiles: OutputFiles{{Name: "leaf", Path: "/out/leaf.txt"}}}
}

// TestInputFilesReducerArtifactRefAccepted: a downstream input_files ref to the
// run: reducer's named output_files artifact VALIDATES with no AWF3007.
func TestInputFilesReducerArtifactRefAccepted(t *testing.T) {
	ld := makeLD(&Workflow{ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&Map{Over: Expr("{{ input.xs }}"), As: "u", Container: "c", Concurrency: intPtr(1),
				Body: NodeList{reduceBodyScan()},
				Reduce: &Reduce{Run: "true", Container: "c",
					OutputFiles: OutputFiles{{Name: "report", Path: "/out/report.md"}}},
			},
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/report.md": "step.scan.files.report"}},
		},
		InputSchema: &JSONSchema{"type": "object", "additionalProperties": false,
			"required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array"}}},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3007")
}

// TestInputFilesReducerNonDeclaredArtifactErrors: a ref to a body-step artifact
// name the reducer does NOT declare in its output_files still errors (AWF3007).
func TestInputFilesReducerNonDeclaredArtifactErrors(t *testing.T) {
	ld := makeLD(&Workflow{ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&Map{Over: Expr("{{ input.xs }}"), As: "u", Container: "c", Concurrency: intPtr(1),
				Body: NodeList{reduceBodyScan()},
				Reduce: &Reduce{Run: "true", Container: "c",
					OutputFiles: OutputFiles{{Name: "report", Path: "/out/report.md"}}},
			},
			// "leaf" is a BODY-step artifact, NOT a reducer artifact → AWF3007.
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/leaf.txt": "step.scan.files.leaf"}},
		},
		InputSchema: &JSONSchema{"type": "object", "additionalProperties": false,
			"required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array"}}},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

// TestInputFilesQuorumReducerHasNoArtifactsErrors: a quorum reducer produces a
// synthetic typed output with NO artifacts, so any .files.<name> ref into it errors.
func TestInputFilesQuorumReducerHasNoArtifactsErrors(t *testing.T) {
	ld := makeLD(&Workflow{ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&Map{Over: Expr("{{ input.xs }}"), As: "u", Container: "c", Concurrency: intPtr(1),
				Body: NodeList{
					&CodeStep{ID: "scan", Container: "c", Run: "true",
						OutputSchema: &JSONSchema{"type": "object", "additionalProperties": false,
							"required": []any{"agree"}, "properties": map[string]any{"agree": map[string]any{"type": "boolean"}}},
						OutputFiles: OutputFiles{{Name: "leaf", Path: "/out/leaf.txt"}}},
				},
				Reduce: &Reduce{Quorum: reduceRatio("2"), Field: "agree"},
			},
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/leaf.txt": "step.scan.files.leaf"}},
		},
		InputSchema: &JSONSchema{"type": "object", "additionalProperties": false,
			"required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array"}}},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

// TestInputFilesNonReduceMapStillRejectsCrossScope: a non-reduce map's body
// artifact remains unreachable from outside the map (AWF3007) — unchanged.
func TestInputFilesNonReduceMapStillRejectsCrossScope(t *testing.T) {
	ld := makeLD(&Workflow{ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&Map{Over: Expr("{{ input.xs }}"), As: "u", Container: "c", Concurrency: intPtr(1),
				Body: NodeList{reduceBodyScan()},
			},
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/leaf.txt": "step.scan.files.leaf"}},
		},
		InputSchema: &JSONSchema{"type": "object", "additionalProperties": false,
			"required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array"}}},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}
