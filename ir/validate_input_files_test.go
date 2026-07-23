package ir

import "testing"

// recon is a producer step declaring a NAMED output_files artifact "report".
func reconProducer() *CodeStep {
	return &CodeStep{
		ID:          "recon",
		Container:   "c",
		Run:         "true",
		OutputFiles: OutputFiles{{Name: "report", Path: "/out/report.md"}},
	}
}

func TestInputFilesValidRefNoError(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			reconProducer(),
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"}},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3007")
}

func TestInputFilesUndeclaredProducerReportsAWF3007(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/report.md": "step.nope.files.report"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

func TestInputFilesUndeclaredArtifactNameReportsAWF3007(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			reconProducer(),
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/report.md": "step.recon.files.missing"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

func TestInputFilesTemplateRefReportsAWF3007(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			reconProducer(),
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/report.md": "{{ step.recon.files.report }}"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

func TestInputFilesAssetRefAcceptedWhenDeclared(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Assets:     map[string]string{"fixture": "fixtures/input.json"},
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/input.json": "asset.fixture"}},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3007")
}

func TestInputFilesUnknownAssetReportsAWF3007(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Assets:     map[string]string{"fixture": "fixtures/input.json"},
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/input.json": "asset.missing"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

func TestInputFilesWorkflowInputFileRefAccepted(t *testing.T) {
	wf := &Workflow{
		ID: "child", Version: 1,
		InputFiles: WorkflowInputFiles{"report": {}},
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "use", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/report.json": "input.files.report"}},
		},
	}
	assertNoErrorCode(t, Validate(makeLD(wf)), "AWF3007")
}

func TestInputFilesWorkflowInputFileRefRejectsUndeclaredName(t *testing.T) {
	wf := &Workflow{
		ID: "child", Version: 1,
		InputFiles: WorkflowInputFiles{"report": {}},
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "use", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/report.json": "input.files.missing"}},
		},
	}
	assertErrorAt(t, Validate(makeLD(wf)), "AWF3007", "use")
}

func TestInputFilesAssetRefAcceptedForFileAndDirectoryDeclarations(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Assets: map[string]string{
			"fixture_file": "fixtures/input.json",
			"fixtures":     "fixtures",
		},
		Containers: awf5003Container(),
		Graph: NodeList{
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{
					"/work/input.json": "asset.fixture_file",
					"/work/fixtures":   "asset.fixtures",
				}},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3007")
}

func TestInputFilesNonAbsoluteDstReportsAWF3007(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			reconProducer(),
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"work/report.md": "step.recon.files.report"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

func TestInputFilesDotDotDstReportsAWF3007(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			reconProducer(),
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/../etc/report.md": "step.recon.files.report"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

func TestInputFilesOverlappingDstReportsAWF3007(t *testing.T) {
	for name, inputFiles := range map[string]map[string]string{
		"parent child": {
			"/work":        "asset.a",
			"/work/report": "asset.b",
		},
		"root child": {
			"/":     "asset.a",
			"/work": "asset.b",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ld := makeLD(&Workflow{
				ID: "x", Version: 1,
				Assets: map[string]string{
					"a": "a.txt",
					"b": "b.txt",
				},
				Containers: awf5003Container(),
				Graph: NodeList{
					&CodeStep{ID: "hunt", Container: "c", Run: "true", InputFiles: inputFiles},
				},
			})
			assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
		})
	}
}

// A named artifact produced inside a gate is consumable after the gate. At
// runtime it resolves to the accepted attempt's committed file. Scalar step refs
// remain gate-scoped; this promotion is specific to input_files artifacts.
func TestInputFilesProducerInsidePassedGateAllowed(t *testing.T) {
	schema := &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&Gate{
				Generate:    NodeList{reconProducer()},
				Evaluate:    NodeList{&CodeStep{ID: "judge", Container: "c", Run: "true", OutputSchema: schema}},
				Until:       Expr("{{ step.judge.exit_code == 0 }}"),
				MaxAttempts: 2,
			},
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"}},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3007")
}

// A named artifact produced by a gate's EVALUATOR (evaluate:) is NOT promoted
// out of the gate — the verdict stays gate-internal by design, same as the
// scalar channel (TestGateEvaluatorRefFromOutsideRejected). Before this fix,
// validateParsedNamedArtifactRef peeled ANY gate scope via isGateScope, so
// this validated clean. The runtime counterpart is
// engine/artifact_scope.go's passedGateArtifactRuntimePath — NOT
// engine/scope.go's stepRuntimePath, which the artifact channel never reaches
// at all (ResolveArtifactPath calls passedGateArtifactRuntimePath directly and
// only falls through to stepRuntimePath when it declines). Until
// passedGateArtifactRuntimePath got its own generate:-only check, it had no
// backstop for this case either — this validation fix and that runtime fix now
// enforce the same rule independently.
func TestInputFilesRefIntoGateEvaluateFromOutsideRejected(t *testing.T) {
	schema := &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&Gate{
				Generate: NodeList{reconProducer()},
				Evaluate: NodeList{&CodeStep{
					ID: "judge", Container: "c", Run: "true", OutputSchema: schema,
					OutputFiles: OutputFiles{{Name: "verdict", Path: "/out/verdict.json"}},
				}},
				Until:       Expr("{{ step.judge.exit_code == 0 }}"),
				MaxAttempts: 2,
			},
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/verdict.json": "step.judge.files.verdict"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

// A producer at map[0].body.gate[0].generate.x, read from OUTSIDE the map, is
// map opacity reopened through a gate: SingleMapBodyShape returns false for
// any path containing "gate[", so the old one-shot opaqueScopePrefix +
// isGateScope check found only the innermost (gate) scope and peeled it,
// validating clean. blockingScope walks outward and still blocks on the
// enclosing map. This is a validation-only guarantee for the artifact
// channel: engine/artifact_scope.go's passedGateArtifactRuntimePath keys off
// the innermost gate (gateScopePrefix) the same way the old validator did — it
// has no enclosing-map-boundary check of its own (out of scope for this fix;
// see engine.Scope.stepRuntimePath's map arm for the scalar channel's
// principled version) — so this workflow shape must never reach the runtime
// unrejected, which is exactly what this test pins.
func TestInputFilesRefIntoGateInsideMapFromOutsideMapRejected(t *testing.T) {
	schema := &JSONSchema{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "additionalProperties": false}
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers:  awf5003Container(),
		InputSchema: &JSONSchema{"type": "object", "required": []any{"xs"}, "properties": map[string]any{"xs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "additionalProperties": false},
		Graph: NodeList{
			&Map{Over: Expr("{{ input.xs }}"), As: "x", Container: "c", Concurrency: intPtr(1), Body: NodeList{
				&Gate{
					Generate:    NodeList{reconProducer()},
					Evaluate:    NodeList{&CodeStep{ID: "judge", Container: "c", Run: "true", OutputSchema: schema}},
					Until:       Expr("{{ step.judge.exit_code == 0 }}"),
					MaxAttempts: 2,
				},
			}},
			&CodeStep{ID: "hunt", Container: "c", Run: "true",
				InputFiles: map[string]string{"/work/report.md": "step.recon.files.report"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

// Agent-step consumer also validates input_files.
func TestInputFilesAgentConsumerUndeclaredReportsAWF3007(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		Containers: awf5003Container(),
		Graph: NodeList{
			&AgentStep{ID: "hunt", Container: "c", With: RawConfig{"prompt": "go"},
				InputFiles: map[string]string{"/work/report.md": "step.nope.files.report"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "hunt")
}

// A CONTAINERLESS agent step (uses: awf/llm, no container:) keys input_files by a
// logical LABEL, not an in-container absolute path. The label must match
// stepIDPattern (the same name charset as workflow input_files names), so a bare
// label like "doc" is accepted, not rejected as a non-absolute path.
func TestInputFiles_ContainerlessLabelKeyAcceptedInCode(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		InputFiles: WorkflowInputFiles{"document": {}},
		Graph: NodeList{
			&AgentStep{ID: "ask", Uses: "awf/llm", With: RawConfig{"prompt": "go"},
				InputFiles: map[string]string{"doc": "input.files.document"}},
		},
	})
	assertNoErrorCode(t, Validate(ld), "AWF3007")
}

// A container-backed step still requires an absolute, clean input_files key — a
// bare relative label like "doc" is rejected with AWF3007.
func TestInputFiles_ContainerBackedStillRequiresAbsPathInCode(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		InputFiles: WorkflowInputFiles{"document": {}},
		Containers: awf5003Container(),
		Graph: NodeList{
			&AgentStep{ID: "ask", Container: "c", With: RawConfig{"prompt": "go"},
				InputFiles: map[string]string{"doc": "input.files.document"}},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "ask")
}

// A non-empty input_files on a containerless step is accompanied by an
// AWF2003 WARNING: static validation can't check per-file format/provider
// compatibility (that happens at run time when the bytes + provider are known).
func TestInputFiles_ContainerlessEmitsRuntimeCompatWarning(t *testing.T) {
	ld := makeLD(&Workflow{
		ID: "x", Version: 1,
		InputFiles: WorkflowInputFiles{"document": {}},
		Graph: NodeList{
			&AgentStep{ID: "ask", Uses: "awf/llm", With: RawConfig{"prompt": "go"},
				InputFiles: map[string]string{"doc": "input.files.document"}},
		},
	})
	assertWarningAt(t, Validate(ld), "AWF2003", "ask")
}
