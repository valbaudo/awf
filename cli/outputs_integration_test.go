package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
)

// e2eGateOutputsWorkflow is a minimal gate where:
//   - generate step "draft" declares an output_schema (so the engine stores a
//     content-addressed blob on commit and sets OutputsRef in node.completed)
//   - evaluate step "judge" returns {"verified":true,"feedback":"ok"} → gate passes
//     on attempt 1
//
// The workflow uses a static image so selectRunBackendForLoadedDefinition
// auto-selects docker; resolveBackend then returns the injected fake (r.Backend
// is non-nil). This mirrors the pattern used by TestCLIRunOnGateFixture.
const e2eGateOutputsWorkflow = `workflow: e2e-gate-outputs
version: 1
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - gate:
      generate:
        - id: draft
          container: c0
          run: echo draft
          output_schema:
            type: object
            additionalProperties: false
            required: [label]
            properties:
              label: { type: string }
      evaluate:
        - id: judge
          container: c0
          run: echo judge
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, feedback]
            properties:
              verified: { type: boolean }
              feedback: { type: string }
      until: "{{ evaluate.verified }}"
      max_attempts: 1
`

// TestOutputsStepE2ENestedGateRead runs a gate (fake backend) that commits a
// generate step at gate[0].attempt-1.generate.draft, then reads it via the CLI.
//
// This is the Part-A end-to-end test: engine.Run writes a real on-disk journal
// + blobs, then cliOutputs reads the gate-internal step by runtime address.
//
// Pattern mirrors TestCLIRunOnGateFixture (cli/run_test.go:985) for the run
// half and TestOutputsStepReadsNestedGatePath (cli/outputs_test.go:90) for the
// read half, composing them into a single true end-to-end flow.
func TestOutputsStepE2ENestedGateRead(t *testing.T) {
	fake := container.NewFake()
	// draft generate step: returns typed output {"label":"x"} via AWFOutput.
	// Engine validates against output_schema, stores blob, sets OutputsRef in
	// the node.completed event.
	fake.ProgramExec("echo draft", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"label":"x"}`),
	}, nil)
	// judge evaluate step: verified:true → until resolves → gate passes.
	fake.ProgramExec("echo judge", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"verified":true,"feedback":"ok"}`),
	}, nil)

	tmp := t.TempDir()
	wfPath := filepath.Join(tmp, "gate.yaml")
	if err := os.WriteFile(wfPath, []byte(e2eGateOutputsWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	stateDir := t.TempDir()
	runner := &Runner{
		Backend: fake,
		IDGen:   &clock.Fake{IDs: []string{"r1"}},
	}

	var runOut, runErr bytes.Buffer
	if rc := runner.Run([]string{"run", "--state-dir", stateDir, wfPath}, &runOut, &runErr); rc != ExitOK {
		t.Fatalf("awf run: rc=%d; stdout=%s stderr=%s", rc, runOut.String(), runErr.String())
	}

	var out, errb bytes.Buffer
	rc := cliOutputs([]string{"r1", "--step", "gate[0].attempt-1.generate.draft", "--state-dir", stateDir}, &out, &errb)
	if rc != ExitOK {
		t.Fatalf("rc=%d (want 0); stderr=%s", rc, errb.String())
	}
	if !strings.Contains(out.String(), `"label"`) {
		t.Fatalf("stdout=%q, want generate step's typed output", out.String())
	}
}
