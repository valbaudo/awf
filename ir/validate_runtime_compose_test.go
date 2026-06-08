package ir

import (
	"strings"
	"testing"
)

func composeProducer() *CodeStep {
	return &CodeStep{
		ID:        "lab_gen",
		Container: "runner",
		Run:       "./generate-lab.sh",
		OutputSchema: &JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"service"},
			"properties": map[string]any{
				"service": map[string]any{"type": "string"},
			},
		},
		OutputFiles: OutputFiles{{Name: "compose", Path: "/work/lab/compose.yml"}},
	}
}

func runtimeComposeWF(body NodeList) *Workflow {
	return &Workflow{
		ID: "runtime-compose", Version: 1,
		Containers: map[string]Container{
			"runner": {Image: "oci://example.com/runner@sha256:" + strings.Repeat("0", 64)},
		},
		Graph: NodeList{
			composeProducer(),
			&Compose{
				As:      "lab",
				From:    "step.lab_gen.files.compose",
				Service: "{{ step.lab_gen.service }}",
				Body:    body,
			},
		},
	}
}

func TestRuntimeComposeScopedHandleAllowedInBody(t *testing.T) {
	ld := makeLD(runtimeComposeWF(NodeList{
		&CodeStep{ID: "smoke", Container: "lab", Run: "true"},
		&CodeStep{ID: "api", Container: "lab:api", Run: "true"},
	}))
	diags := Validate(ld)
	assertNoError(t, diags)
	assertNoErrorCode(t, diags, "AWF1009")
	assertNoErrorCode(t, diags, "AWF3007")
}

func TestRuntimeComposeScopedHandleRejectedOutsideBody(t *testing.T) {
	wf := runtimeComposeWF(NodeList{
		&CodeStep{ID: "smoke", Container: "lab", Run: "true"},
	})
	wf.Graph = append(wf.Graph, &CodeStep{ID: "outside", Container: "lab", Run: "true"})

	diags := Validate(makeLD(wf))
	assertErrorAt(t, diags, "AWF1009", "outside")
}

func TestRuntimeComposeFromMustNameDeclaredOutputFile(t *testing.T) {
	wf := runtimeComposeWF(NodeList{
		&CodeStep{ID: "smoke", Container: "lab", Run: "true"},
	})
	wf.Graph[1].(*Compose).From = "step.lab_gen.files.missing"

	diags := Validate(makeLD(wf))
	assertErrorAt(t, diags, "AWF3007", "compose[1]")
}

func TestRuntimeComposeFromProducerMustPrecedeCompose(t *testing.T) {
	producer := composeProducer()
	compose := &Compose{
		As:      "lab",
		From:    "step.lab_gen.files.compose",
		Service: "{{ step.lab_gen.service }}",
		Body: NodeList{
			&CodeStep{ID: "smoke", Container: "lab", Run: "true"},
		},
	}
	wf := runtimeComposeWF(NodeList{})
	wf.Graph = NodeList{compose, producer}

	diags := Validate(makeLD(wf))
	assertErrorAt(t, diags, "AWF3007", "compose[0]")
}

func TestRuntimeComposeRejectsEmptyBody(t *testing.T) {
	wf := runtimeComposeWF(NodeList{})
	diags := Validate(makeLD(wf))
	assertErrorAt(t, diags, "AWF1038", "compose[1]")
}

func TestRuntimeComposeRejectsAsCollision(t *testing.T) {
	wf := runtimeComposeWF(NodeList{
		&CodeStep{ID: "smoke", Container: "lab", Run: "true"},
	})
	wf.Graph[1].(*Compose).As = "runner"

	diags := Validate(makeLD(wf))
	assertErrorAt(t, diags, "AWF1038", "compose[1]")
}
