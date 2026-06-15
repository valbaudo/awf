package awfllm_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
)

type geminiCacheFake struct {
	mu                sync.Mutex
	getStatus         int    // status for GET cachedContents/{name}
	createName        string // name returned by POST cachedContents
	gets, posts, gens int
}

func (f *geminiCacheFake) rt() http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/cachedContents/"):
			f.gets++
			st := f.getStatus
			if st == 0 {
				st = 200
			}
			if st != 200 {
				return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(`{"error":{"status":"NOT_FOUND"}}`)), Header: http.Header{}}, nil
			}
			return jsonResponse(`{"name":"` + f.createName + `"}`), nil
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cachedContents"):
			f.posts++
			return jsonResponse(`{"name":"` + f.createName + `"}`), nil
		default: // :generateContent
			f.gens++
			return jsonResponse(`{"candidates":[{"content":{"parts":[{"text":"{\"ok\":1}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":3,"cachedContentTokenCount":40}}`), nil
		}
	})
}

func TestEnsureGeminiCache_CreateThenReuse(t *testing.T) {
	f := &geminiCacheFake{createName: "cachedContents/xyz"}
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: f.rt()}))
	cfg := awfllm.ReqConfigForTest{Provider: "gemini", BaseURL: "https://generativelanguage.googleapis.com", Model: "gemini-2.5-pro", APIKey: "k"}
	files := []agent.InputFile{{Name: "d", MIME: "application/pdf", Content: []byte("%PDF-1.7")}}

	name, err := awfllm.EnsureGeminiCacheForTest(a, context.Background(), cfg, files)
	if err != nil || name != "cachedContents/xyz" {
		t.Fatalf("create: name=%q err=%v", name, err)
	}
	if f.posts != 1 {
		t.Errorf("first call must POST once, got %d", f.posts)
	}
	name2, err := awfllm.EnsureGeminiCacheForTest(a, context.Background(), cfg, files)
	if err != nil || name2 != "cachedContents/xyz" {
		t.Fatalf("reuse: name=%q err=%v", name2, err)
	}
	if f.posts != 1 {
		t.Errorf("reuse must NOT create again, posts=%d", f.posts)
	}
	if f.gets != 1 {
		t.Errorf("reuse must GET-verify once, gets=%d", f.gets)
	}
}

func TestEnsureGeminiCache_ExpiredHandleRecreated(t *testing.T) {
	f := &geminiCacheFake{createName: "cachedContents/n1"}
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: f.rt()}))
	cfg := awfllm.ReqConfigForTest{Provider: "gemini", BaseURL: "https://generativelanguage.googleapis.com", Model: "gemini-2.5-pro", APIKey: "k"}
	files := []agent.InputFile{{Name: "d", MIME: "application/pdf", Content: []byte("%PDF-1.7")}}

	if _, err := awfllm.EnsureGeminiCacheForTest(a, context.Background(), cfg, files); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.getStatus = 404
	f.createName = "cachedContents/n2"
	f.mu.Unlock()
	name, err := awfllm.EnsureGeminiCacheForTest(a, context.Background(), cfg, files)
	if err != nil {
		t.Fatal(err)
	}
	if name != "cachedContents/n2" {
		t.Errorf("expired handle must be recreated, got %q", name)
	}
	if f.posts != 2 {
		t.Errorf("expected 2 creates (initial + after 404), got %d", f.posts)
	}
}

func TestEnsureGeminiCache_NoDoubleCreateUnderConcurrency(t *testing.T) {
	f := &geminiCacheFake{createName: "cachedContents/once"}
	a, _ := awfllm.New(awfllm.WithHTTPClient(&http.Client{Transport: f.rt()}))
	cfg := awfllm.ReqConfigForTest{Provider: "gemini", BaseURL: "https://generativelanguage.googleapis.com", Model: "gemini-2.5-pro", APIKey: "k"}
	files := []agent.InputFile{{Name: "d", MIME: "application/pdf", Content: []byte("%PDF-1.7")}}

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() { defer wg.Done(); _, _ = awfllm.EnsureGeminiCacheForTest(a, context.Background(), cfg, files) }()
	}
	wg.Wait()
	if f.posts != 1 {
		t.Errorf("concurrent first-use must create exactly once, got %d POSTs", f.posts)
	}
}

func TestGeminiCacheKey_ContentAddressed(t *testing.T) {
	f1 := []agent.InputFile{{Name: "a", MIME: "application/pdf", Content: []byte("DOC-A")}}
	f1dup := []agent.InputFile{{Name: "different-name", MIME: "application/pdf", Content: []byte("DOC-A")}}
	f2 := []agent.InputFile{{Name: "a", MIME: "application/pdf", Content: []byte("DOC-B")}}

	k := awfllm.GeminiCacheKeyForTest("gemini-2.5-pro", "sys", f1)
	if k != awfllm.GeminiCacheKeyForTest("gemini-2.5-pro", "sys", f1dup) {
		t.Error("same model+sys+bytes+mime must produce the same key regardless of filename")
	}
	if k == awfllm.GeminiCacheKeyForTest("gemini-2.5-pro", "sys", f2) {
		t.Error("different document bytes must produce a different key")
	}
	if k == awfllm.GeminiCacheKeyForTest("gemini-2.5-flash", "sys", f1) {
		t.Error("different model must produce a different key")
	}
	if k == awfllm.GeminiCacheKeyForTest("gemini-2.5-pro", "OTHER-sys", f1) {
		t.Error("different systemInstruction must produce a different key (it is baked into the cache)")
	}
	if k == "" {
		t.Error("key must be non-empty")
	}
}
