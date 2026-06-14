package awfllm_test

import (
	"context"
	"encoding/json"
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
	// The stable document part must precede the varying prompt text in the
	// contents array so the document is the cacheable common prefix (implicit
	// caching). "extract" is the prompt text.
	if di, pi := strings.Index(gotBody, `"inlineData"`), strings.Index(gotBody, "extract"); di < 0 || pi < 0 || di > pi {
		t.Errorf("inlineData (doc) must come BEFORE the prompt text in contents: inlineData@%d prompt@%d\n%s", di, pi, gotBody)
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

// TestCallGemini_StructuredOutputOff — Fix C: structured_output:off must mean
// "no native structured output" for the Gemini transport too. With off, even when
// an output_schema is present, the request body must OMIT responseMimeType and
// _responseJsonSchema (prompt-restate only, via assemblePrompt's N2 floor).
func TestCallGemini_StructuredOutputOff(t *testing.T) {
	const resp = `{"candidates":[{"content":{"parts":[{"text":"{\"name\":\"x\"}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"cachedContentTokenCount":0}}`
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResponse(resp), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))
	cfg := awfllm.ReqConfigForTest{
		Provider:         "gemini",
		BaseURL:          "https://generativelanguage.googleapis.com",
		Model:            "gemini-3.5-flash",
		APIKey:           "k",
		StructuredOutput: "off",
	}
	_, _, _, _, err := a.StreamWithFilesForTest(
		context.Background(), cfg, "extract", &ir.JSONSchema{"type": "object"}, nil, nil,
		func(string, []byte) {})
	if err != nil {
		t.Fatalf("callGemini: %v", err)
	}
	if strings.Contains(gotBody, "responseMimeType") {
		t.Errorf("structured_output:off must OMIT responseMimeType: %s", gotBody)
	}
	if strings.Contains(gotBody, "_responseJsonSchema") {
		t.Errorf("structured_output:off must OMIT _responseJsonSchema: %s", gotBody)
	}
}

// TestCallGemini_ThreadRendered — finding #1: the Gemini transport advertises a
// global Threaded:true but previously DROPPED the engine-assembled thread. A
// continues: chain landing on a gemini step must replay the prior turns. Gemini's
// generateContent contents[] is multi-turn natively: each prior turn becomes a
// {role:user} then {role:model} pair (Gemini uses "model", NOT "assistant"), in
// chronological order, with the CURRENT user turn last.
func TestCallGemini_ThreadRendered(t *testing.T) {
	const resp = `{"candidates":[{"content":{"parts":[{"text":"a2"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"cachedContentTokenCount":0}}`
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResponse(resp), nil
	})
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: rt}))

	cfg := awfllm.ReqConfigForTest{
		Provider: "gemini",
		BaseURL:  "https://generativelanguage.googleapis.com",
		Model:    "gemini-3.5-flash",
		APIKey:   "k",
	}
	thread := []agent.ThreadTurn{{User: "q1", Assistant: "a1"}}
	_, _, _, _, err := a.StreamWithFilesForTest(
		context.Background(), cfg, "q2", nil, thread, nil,
		func(string, []byte) {})
	if err != nil {
		t.Fatalf("callGemini: %v", err)
	}

	// Decode the contents[] and assert role/text ordering: prior turn user/model
	// pair BEFORE the current user turn.
	var req struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("unmarshal request body: %v (%s)", err, gotBody)
	}
	if len(req.Contents) != 3 {
		t.Fatalf("contents len = %d, want 3 (prior user, prior model, current user): %s", len(req.Contents), gotBody)
	}
	type rt0 struct{ role, text string }
	got := []rt0{}
	for _, c := range req.Contents {
		text := ""
		if len(c.Parts) > 0 {
			text = c.Parts[0].Text
		}
		got = append(got, rt0{role: c.Role, text: text})
	}
	want := []rt0{
		{role: "user", text: "q1"},
		{role: "model", text: "a1"}, // Gemini's assistant role is "model", NOT "assistant"
		{role: "user", text: "q2"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("contents[%d] = %+v, want %+v (full: %s)", i, got[i], want[i], gotBody)
		}
	}
	// Defensive: the assistant role must never be "assistant" on the Gemini wire.
	if strings.Contains(gotBody, `"role":"assistant"`) {
		t.Errorf("Gemini wire must use role \"model\" not \"assistant\": %s", gotBody)
	}
}

// TestCallGemini_UnknownModelHasNoCost confirms a Gemini model absent from
// pricing/rates.json yields NO derived cost (pricing.Derive ok=false → buildResult
// leaves Metrics.Cost zero-valued: empty Source, zero Total). Cost is left
// UNREPORTED for Gemini by this fall-through, no special-casing. Uses a
// deliberately NON-EXISTENT model id so it keeps exercising the absent→no-cost
// path even though the popular Gemini models are now in the embedded table.
func TestCallGemini_UnknownModelHasNoCost(t *testing.T) {
	const unpricedModel = "gemini-0.0-doesnotexist"
	usage := awfllm.NewUsageForTest(1200, 15, 0)
	res, err := awfllm.BuildResultForTest(
		`{"name":"x"}`, usage, unpricedModel, pricing.Default(),
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
	if res.Metrics.Model != unpricedModel {
		t.Errorf("Metrics.Model = %q, want %q", res.Metrics.Model, unpricedModel)
	}
}
