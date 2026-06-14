package awfllm_test

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// minimalPDF is a valid single-page PDF with one "Hello AWF" text element.
// Generated as a hand-crafted %PDF-1.4 document that satisfies the cross-reference
// table contract. The assertion is that the Gemini call succeeds and tokens are
// counted — extraction accuracy is not tested.
var minimalPDF = func() []byte {
	// Minimal valid PDF 1.4:
	//   obj 1 = catalog, obj 2 = pages, obj 3 = page, obj 4 = font, obj 5 = content stream
	const raw = "%PDF-1.4\n" +
		"1 0 obj\n<</Type /Catalog /Pages 2 0 R>>\nendobj\n" +
		"2 0 obj\n<</Type /Pages /Kids [3 0 R] /Count 1>>\nendobj\n" +
		"3 0 obj\n<</Type /Page /Parent 2 0 R /MediaBox [0 0 612 792]\n" +
		"  /Contents 5 0 R /Resources <</Font <</F1 4 0 R>>>>>>\nendobj\n" +
		"4 0 obj\n<</Type /Font /Subtype /Type1 /BaseFont /Helvetica>>\nendobj\n" +
		"5 0 obj\n<</Length 44>>\nstream\nBT /F1 12 Tf 72 720 Td (Hello AWF) Tj ET\nendstream\nendobj\n" +
		"xref\n0 6\n" +
		"0000000000 65535 f \n" +
		"0000000009 00000 n \n" +
		"0000000058 00000 n \n" +
		"0000000115 00000 n \n" +
		"0000000266 00000 n \n" +
		"0000000340 00000 n \n" +
		"trailer\n<</Size 6 /Root 1 0 R>>\n" +
		"startxref\n450\n%%EOF\n"
	return []byte(raw)
}()

// TestGeminiLive_PDF is a GATED live smoke test that hits the real Gemini API.
// It is skipped by default (CI has no API key and no network access to the live
// API). To run it locally:
//
//	AWF_LLM_LIVE=1 GEMINI_API_KEY=<key> go test ./agent/awfllm/ -run TestGeminiLive_PDF -v
//
// What it verifies empirically (cannot be confirmed with a fake RoundTripper):
//   - usageMetadata field casing: promptTokenCount / candidatesTokenCount /
//     cachedContentTokenCount (usage.Input > 0 proves the JSON struct tag is right)
//   - _responseJsonSchema is accepted by the live API (no 400 on the schema field)
//   - inlineData bare-base64 + mimeType for a real PDF is accepted
func TestGeminiLive_PDF(t *testing.T) {
	if os.Getenv("AWF_LLM_LIVE") != "1" {
		t.Skip("skipping live Gemini smoke test: AWF_LLM_LIVE is not set to 1")
	}
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("skipping live Gemini smoke test: GEMINI_API_KEY is empty")
	}

	// Model to use. The runner can override via AWF_LLM_LIVE_MODEL since
	// model availability changes over time. Default: gemini-2.5-flash (June 2026).
	model := os.Getenv("AWF_LLM_LIVE_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}

	// Build a REAL adapter with the live API key in the env allowlist.
	// No fake RoundTripper — this must reach the live Gemini API.
	a, err := awfllm.New(
		awfllm.WithEnv(map[string]string{"GEMINI_API_KEY": apiKey}),
		awfllm.WithHTTPClient(&http.Client{}),
	)
	if err != nil {
		t.Fatalf("awfllm.New: %v", err)
	}

	schema := &ir.JSONSchema{
		"type": "object",
		"properties": map[string]any{
			"greeting": map[string]any{"type": "string"},
		},
		"required": []any{"greeting"},
	}

	inv := agent.AgentInvocation{
		NodePath: "test[0]",
		Uses:     awfllm.AdapterRef,
		With: ir.RawConfig{
			"provider": "gemini",
			"model":    model,
			"prompt":   "The attached PDF contains a greeting. Return it as the 'greeting' field.",
		},
		OutputSchema: schema,
		InputFiles: []agent.InputFile{
			{Name: "doc", MIME: "application/pdf", Content: minimalPDF},
		},
	}

	// γ drain contract: range over events first, THEN read the outcome channel.
	events, outcomeCh, err := a.Launch(context.Background(), container.Handle{}, inv)
	if err != nil {
		t.Fatalf("Launch (pre-launch err): %v", err)
	}
	for range events { //nolint:revive // drain required by the γ contract
	}
	outcome := <-outcomeCh

	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}

	// usageMetadata casing check: promptTokenCount must have parsed to a non-zero
	// Input count (a PDF with text will always consume prompt tokens).
	if outcome.Result.Metrics.Tokens.Input == 0 {
		t.Errorf("Metrics.Tokens.Input == 0: usageMetadata.promptTokenCount may not have parsed (check field casing in transport.go)")
	}

	// The live API must return a non-empty typed output (OutputSchema is set, so
	// Launch runs buildResult and populates Output; an empty map means the parse
	// recovered nothing, which is also a signal of a bad response).
	if len(outcome.Result.Output) == 0 {
		t.Errorf("result Output is empty: call succeeded but returned no typed fields")
	}

	// Model id: callGemini stamps cfg.Model as the wire model; it must be non-empty.
	if outcome.Result.Metrics.Model == "" {
		t.Errorf("Metrics.Model is empty; expected the requested model id to be stamped")
	}
}
