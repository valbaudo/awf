package engine_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// TestRunSuppliesTopLevelInputFiles is the fresh-run delivery half of Task 15:
// a top-level workflow `input_files: { document: {} }` supplied at run start via
// RunOptions.InputFiles must flow through runstate.InputFiles → ictx.inputFiles →
// the root Scope's input.files.<name> resolution, so a CONTAINERLESS awf/llm step
// that declares `input_files: { doc: input.files.document }` actually receives the
// supplied PDF bytes in its AgentInvocation.
//
// Harness mirrors engine/agent_step_test.go (loadAgentSimpleDef + a scripted fake
// adapter + LocalDispatcher driven through the public engine.Run) and the
// containerless awf/llm shape from examples/awf-llm-pdf-extract/workflow.yaml.
func TestRunSuppliesTopLevelInputFiles(t *testing.T) {
	const yaml = `workflow: awf-llm-input-files
version: 1
input_files:
  document: {}
graph:
  - id: extract
    uses: awf/llm
    input_files:
      doc: input.files.document
    with:
      provider: gemini
      model: gemini-2.5-flash
      api_key_env: GEMINI_API_KEY
      prompt: "Summarize the attached document."
    output_schema:
      type: object
      additionalProperties: false
      required: [verdict]
      properties:
        verdict: { type: string }
`
	ld := loadAgentSimpleDef(t, yaml)

	var reg agent.Registry
	fk := fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true}).
		Script(0, fake.Result{Output: map[string]any{"verdict": "ok"}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Containerless step: no container handles required.
	dispatcher := &engine.LocalDispatcher{Resolver: &reg}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	blobs := state.NewInMemoryBlobs()

	pdf := []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< >>\nendobj\n")
	cas, err := blobs.Put(pdf)
	if err != nil {
		t.Fatalf("Put pdf: %v", err)
	}

	rs := engine.NewRunState("r1", "d", nil)
	oc, err := engine.Run(context.Background(), ld, rs, dispatcher, log, blobs, clk, engine.RunOptions{
		Tap:        io.Discard,
		InputFiles: map[string]string{"document": cas},
	})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(calls))
	}
	got := calls[0].InputFiles
	if len(got) != 1 {
		t.Fatalf("adapter InputFiles len = %d, want 1: %+v", len(got), got)
	}
	if got[0].Name != "doc" {
		t.Errorf("InputFiles[0].Name = %q, want %q", got[0].Name, "doc")
	}
	if got[0].MIME != "application/pdf" {
		t.Errorf("InputFiles[0].MIME = %q, want %q", got[0].MIME, "application/pdf")
	}
}

// TestFoldRestoresTopLevelInputFiles is the resume-fold half of Task 15: a
// run.started event whose RunStartedData.InputFiles records the supplied
// top-level workflow input file manifest must be folded back into
// RunState.InputFiles (mirroring how rs.Assets is restored), so resume — where
// RunOptions.InputFiles is nil — still resolves input.files.<name>.
func TestFoldRestoresTopLevelInputFiles(t *testing.T) {
	cas := "awf-d1:sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	want := map[string]string{"document": cas}

	events := []state.Event{
		{
			Seq: 1, Type: engine.EventRunStarted,
			Data: mustJSON(engine.RunStartedData{
				RunID:          "r1",
				WorkflowDigest: "d",
				InputFiles:     want,
			}),
		},
	}

	rs, err := engine.Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(rs.InputFiles) != len(want) {
		t.Fatalf("rs.InputFiles = %+v, want %+v", rs.InputFiles, want)
	}
	if rs.InputFiles["document"] != cas {
		t.Errorf("rs.InputFiles[document] = %q, want %q", rs.InputFiles["document"], cas)
	}
}
