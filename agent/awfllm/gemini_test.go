package awfllm_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/pricing"
)

// jsonResponse returns a canned 200 application/json response (Gemini
// :generateContent is non-streaming in v1).
func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestCallGemini_PDFRequestAndUsage drives the native Gemini generateContent
// transport with one PDF InputFile and a typed output_schema, asserting both the
// REQUEST wire (inlineData bare base64, mimeType, structured-output config,
// x-goog-api-key header, :generateContent URL) and the PARSED result (text from
// candidates[0], usage from usageMetadata, finishReason, wire model = cfg.Model).
func TestCallGemini_PDFRequestAndUsage(t *testing.T) {
	const resp = `{
	  "candidates":[{"content":{"parts":[{"text":"{\"name\":\"x\"}"}]},"finishReason":"STOP"}],
	  "usageMetadata":{"promptTokenCount":1200,"candidatesTokenCount":15,"cachedContentTokenCount":0}
	}`
	var gotBody, gotKey, gotURL string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotKey = r.Header.Get("x-goog-api-key")
		gotURL = r.URL.String()
		// Gemini documents no Idempotency-Key: assert the request omits it.
		if got := r.Header.Get("Idempotency-Key"); got != "" {
			t.Errorf("Idempotency-Key must be absent, got %q", got)
		}
		return jsonResponse(resp), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))

	cfg := awfllm.ReqConfigForTest{
		Provider: "gemini",
		BaseURL:  "https://generativelanguage.googleapis.com",
		Model:    "gemini-3.5-flash",
		APIKey:   "k",
	}
	files := []agent.InputFile{{Name: "doc", MIME: "application/pdf", Content: []byte("%PDF-1.7")}}
	text, usage, model, finish, err := a.StreamWithFilesForTest(
		context.Background(), cfg, "extract", &ir.JSONSchema{"type": "object"}, nil, files,
		func(string, []byte) {})
	if err != nil {
		t.Fatalf("callGemini: %v", err)
	}

	// Parsed result.
	if text != `{"name":"x"}` {
		t.Errorf("text = %q, want %q", text, `{"name":"x"}`)
	}
	if usage.Input != 1200 || usage.Output != 15 || usage.CacheRead != 0 {
		t.Errorf("usage = %+v, want {Input:1200 Output:15 CacheRead:0}", usage)
	}
	if finish != "STOP" {
		t.Errorf("finish = %q, want STOP", finish)
	}
	if model != "gemini-3.5-flash" {
		t.Errorf("model = %q, want gemini-3.5-flash", model)
	}

	// Request wire.
	if gotKey != "k" {
		t.Errorf("x-goog-api-key = %q, want k", gotKey)
	}
	if !strings.Contains(gotURL, "gemini-3.5-flash:generateContent") {
		t.Errorf("URL = %q, want it to contain gemini-3.5-flash:generateContent", gotURL)
	}
	if !strings.Contains(gotBody, `"inlineData"`) {
		t.Errorf("body missing inlineData: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"mimeType":"application/pdf"`) {
		t.Errorf("body missing mimeType application/pdf: %s", gotBody)
	}
	// inlineData carries BARE base64 (no data: prefix). %PDF-1.7 → JVBERi0xLjc=.
	if !strings.Contains(gotBody, `"data":"JVBERi0xLjc="`) {
		t.Errorf("body missing bare base64 data (no data: prefix): %s", gotBody)
	}
	if strings.Contains(gotBody, "data:application/pdf") {
		t.Errorf("inlineData must NOT use a data: URI prefix: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"responseMimeType":"application/json"`) {
		t.Errorf("body missing responseMimeType: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"_responseJsonSchema"`) {
		t.Errorf("body missing _responseJsonSchema: %s", gotBody)
	}
}

// TestCallGemini_UnknownModelHasNoCost confirms a Gemini model absent from
// pricing/rates.json yields NO derived cost (pricing.Derive ok=false → buildResult
// leaves Metrics.Cost zero-valued: empty Source, zero Total). Cost is left
// UNREPORTED for Gemini by this fall-through, no special-casing.
func TestCallGemini_UnknownModelHasNoCost(t *testing.T) {
	usage := awfllm.NewUsageForTest(1200, 15, 0)
	res, err := awfllm.BuildResultForTest(
		`{"name":"x"}`, usage, "gemini-3.5-flash", pricing.Default(),
		agent.AgentInvocation{OutputSchema: &ir.JSONSchema{"type": "object"}},
	)
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if res.Metrics.Cost.Source != "" {
		t.Errorf("Cost.Source = %q, want empty (unknown model → no derived cost)", res.Metrics.Cost.Source)
	}
	if res.Metrics.Cost.Total != 0 {
		t.Errorf("Cost.Total = %v, want 0 (unknown model → no derived cost)", res.Metrics.Cost.Total)
	}
	// Sanity: the wire model id is still stamped even on a pricing miss.
	if res.Metrics.Model != "gemini-3.5-flash" {
		t.Errorf("Metrics.Model = %q, want gemini-3.5-flash", res.Metrics.Model)
	}
}
