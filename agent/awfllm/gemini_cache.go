package awfllm

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/valbaudo/awf/agent"
)

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
