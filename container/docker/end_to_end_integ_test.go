//go:build integ

package docker

import (
	"context"
	"testing"

	cont "github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// TestE2E_AWFOutputContractAgainstRealDocker exercises the full
// engine→docker.Backend pipeline that ships with slice 4.2:
//
//  1. LocalDispatcher injects AWF_OUTPUT into Cmd.Env.
//  2. docker.Backend.Exec runs the bash script which writes JSON to "$AWF_OUTPUT".
//  3. LocalDispatcher post-Exec adds the AWF_OUTPUT tempfile to CaptureFiles paths.
//  4. docker.Backend.CaptureFiles reads the file back.
//  5. LocalDispatcher passes those bytes to ValidateAgainstSchema.
//  6. The typed outputs land in DispatchResult.Outputs.
//
// This is the canonical "AWF_OUTPUT works against real Docker" test.
func TestE2E_AWFOutputContractAgainstRealDocker(t *testing.T) {
	cli, backend := newTestBackend(t, "e2e-awfoutput")
	h := newAlpineContainer(t, cli, backend)
	ctx := context.Background()

	d := &engine.LocalDispatcher{
		Backend: backend,
		Handles: map[string]cont.Handle{"lab": h},
	}

	// The script writes JSON to AWF_OUTPUT. The dispatcher pre-creates
	// /tmp/awf/ via the script's mkdir -p (Design Q4).
	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"verified", "detections"},
		"properties": map[string]any{
			"verified":   map[string]any{"type": "boolean"},
			"detections": map[string]any{"type": "integer"},
		},
	}
	intent := engine.NodeIntent{
		Path: "verify",
		Node: &ir.CodeStep{ID: "verify", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:      `mkdir -p /tmp/awf && printf '{"verified":true,"detections":5}\n' > "$AWF_OUTPUT"`,
			OutputSchema: &schema,
		},
	}
	dr, ch, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Dispatcher.Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Errorf("Outcome = %v, want ok (DispatchResult.Err = %v)", dr.Outcome, dr.Err)
	}
	if dr.Outputs["verified"] != true {
		t.Errorf("Outputs.verified = %v, want true", dr.Outputs["verified"])
	}
	if dr.Outputs["detections"] != float64(5) {
		t.Errorf("Outputs.detections = %v, want 5", dr.Outputs["detections"])
	}
	// Drain the chunks channel to avoid blocking goroutines.
	for range ch {
	}
}

// TestE2E_OutputFilesAlongsideAWFOutput verifies that user-declared
// output_files are captured AND distinct from the AWF_OUTPUT tempfile
// (the tempfile is stripped from the user-visible Files slice).
func TestE2E_OutputFilesAlongsideAWFOutput(t *testing.T) {
	cli, backend := newTestBackend(t, "e2e-bothfiles")
	h := newAlpineContainer(t, cli, backend)
	ctx := context.Background()

	d := &engine.LocalDispatcher{
		Backend: backend,
		Handles: map[string]cont.Handle{"lab": h},
	}

	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"k"},
		"properties":           map[string]any{"k": map[string]any{"type": "string"}},
	}
	intent := engine.NodeIntent{
		Path: "produce",
		Node: &ir.CodeStep{ID: "produce", Container: "lab"},
		ResolvedInputs: engine.ResolvedInputs{
			Command:      `mkdir -p /tmp/awf /out && printf '{"k":"v"}\n' > "$AWF_OUTPUT" && printf 'report content' > /out/report.txt`,
			OutputSchema: &schema,
			OutputFiles:  []string{"/out/report.txt"},
		},
	}
	dr, ch, err := d.Run(ctx, intent)
	if err != nil {
		t.Fatalf("Dispatcher.Run: %v", err)
	}
	if dr.Outcome != engine.OutcomeOK {
		t.Fatalf("Outcome = %v, want ok (err = %v)", dr.Outcome, dr.Err)
	}
	if len(dr.Files) != 1 {
		t.Fatalf("Files len = %d, want 1 (AWF_OUTPUT must be stripped from user-visible Files)", len(dr.Files))
	}
	if dr.Files[0].Path != "/out/report.txt" {
		t.Errorf("Files[0].Path = %q, want /out/report.txt", dr.Files[0].Path)
	}
	if string(dr.Files[0].Content) != "report content" {
		t.Errorf("Files[0].Content = %q, want \"report content\"", dr.Files[0].Content)
	}
	if dr.Outputs["k"] != "v" {
		t.Errorf("Outputs.k = %v, want v", dr.Outputs["k"])
	}
	for range ch {
	}
}
