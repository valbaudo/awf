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
