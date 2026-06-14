package awfllm_test

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
)

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
