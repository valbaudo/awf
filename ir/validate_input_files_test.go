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

// Producer inside a gate body, consumer outside it → unreachable scope → AWF3007.
func TestInputFilesProducerInsideGateUnreachableReportsAWF3007(t *testing.T) {
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
