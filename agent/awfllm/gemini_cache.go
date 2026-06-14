package awfllm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/valbaudo/awf/agent"
)

// ensureGeminiCache returns a CachedContent resource name for (model + systemInstruction
// + files), creating one on first use and reusing it within this adapter instance (one
// `awf run`). A stored handle that 404s on GET (expired) is evicted and recreated. Cache
// names live ONLY in this in-process map: never journaled, committed, or resumed (resume
// re-sends the document and re-caches).
func (a *Adapter) ensureGeminiCache(ctx context.Context, cfg reqConfig, files []agent.InputFile) (string, error) {
	key := geminiCacheKey(cfg.Model, cfg.SystemPrompt, files)

	a.geminiCacheMu.RLock()
	name, cached := a.geminiCacheMap[key]
	a.geminiCacheMu.RUnlock()

	if cached {
		ok, err := a.geminiCacheGet(ctx, cfg, name)
		if err != nil {
			return "", err
		}
		if ok {
			return name, nil
		}
		a.geminiCacheMu.Lock()
		if a.geminiCacheMap[key] == name {
			delete(a.geminiCacheMap, key)
		}
		a.geminiCacheMu.Unlock()
	}

	name, err := a.geminiCacheCreate(ctx, cfg, files)
	if err != nil {
		return "", err
	}
	a.geminiCacheMu.Lock()
	a.geminiCacheMap[key] = name
	a.geminiCacheMu.Unlock()
	return name, nil
}

// geminiCacheGet probes a stored CachedContent name. ok=false on 404 (expired);
// error on transport/other-HTTP faults. (GET cachedContents/{name} is verified-real.)
func (a *Adapter) geminiCacheGet(ctx context.Context, cfg reqConfig, name string) (bool, error) {
	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/v1beta/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("x-goog-api-key", cfg.APIKey)
	resp, err := a.clientFor(cfg.TLSInsecure).Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, &apiError{Status: resp.StatusCode, Type: "gemini_error", Body: "cachedContents GET"}
	}
	return true, nil
}

// geminiCacheCreate uploads systemInstruction + document(s) as a CachedContent and
// returns the server-assigned name. The cached object holds the systemInstruction and
// the document; the per-call prompt + thread are sent live on :generateContent (which
// must NOT re-send systemInstruction). The API assigns the name; we do not request one.
func (a *Adapter) geminiCacheCreate(ctx context.Context, cfg reqConfig, files []agent.InputFile) (string, error) {
	parts := make([]map[string]any, 0, len(files))
	for _, f := range files {
		if _, ok := forwardable(providerGemini, f.MIME); !ok {
			return "", unsupportedMIMEErr(f.MIME, "")
		}
		parts = append(parts, map[string]any{"inlineData": map[string]any{
			"mimeType": f.MIME,
			"data":     base64.StdEncoding.EncodeToString(f.Content),
		}})
	}
	ttl := defaultGeminiCacheTTL
	if cfg.GeminiCache != nil && cfg.GeminiCache.TTL != "" {
		ttl = cfg.GeminiCache.TTL
	}
	body := map[string]any{
		"model":    "models/" + cfg.Model,
		"contents": []map[string]any{{"role": "user", "parts": parts}},
		"ttl":      ttl,
	}
	if cfg.SystemPrompt != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": cfg.SystemPrompt}}}
	}
	reqBytes, _ := json.Marshal(body)

	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/v1beta/cachedContents"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", cfg.APIKey)
	resp, err := a.clientFor(cfg.TLSInsecure).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// A 400 here is often "document below the model's minimum cacheable token count
		// (~2048+)". Prefix a hint; keep the *apiError so a 400 invalid_request classifies
		// permanent (classifyLaunchErr), everything else retryable.
		return "", &apiError{Status: resp.StatusCode, Type: geminiErrType(respBytes),
			Body: "CachedContent create failed (the document may be below the model's minimum cacheable token count, ~2048+): " + string(respBytes)}
	}
	var cc struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respBytes, &cc); err != nil {
		return "", err
	}
	if cc.Name == "" {
		return "", fmt.Errorf("agent/awfllm: cachedContents create returned no name")
	}
	return cc.Name, nil
}

// geminiCacheKey is the content address for a CachedContent object: a hash of the
// model, the systemInstruction (it is cached, so it must distinguish caches), and
// each file's MIME and bytes, in order. Two callGemini calls with the same model +
// system prompt + document(s) collide deterministically and reuse one server-side
// cache. The filename is excluded (it does not affect cached tokens).
func geminiCacheKey(model, systemPrompt string, files []agent.InputFile) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(systemPrompt))
	h.Write([]byte{0})
	for _, f := range files {
		h.Write([]byte(f.MIME))
		h.Write([]byte{0})
		h.Write(f.Content)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
