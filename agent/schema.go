package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractJSONObject scans s for a JSON object, using json.Decoder (which
// consumes exactly one well-formed value), returning the LAST object that
// decodes (right bias for an agent that reasons before its final JSON). We do
// NOT schema-validate here (the engine's ValidateOutputMap does, so a
// wrong-but-valid object becomes a retryable schema failure) and do NOT pull in
// a json-repair dependency (it could fabricate a valid-but-wrong object).
//
// This is the "directive+parse" output_schema fidelity tier's parser (see
// docs/runtime-design.md): the adapter asks the model, in-prompt, to make its
// final message a single JSON object, then parses free text for it as a
// backstop against prose/fences. Every adapter on that tier (goose, droid, and
// awfllm's non-API-constrained fallback) calls this one implementation — hoisted
// (F37) from three byte-identical copies. The only prior difference between them
// was the not-found error's package-name prefix, which carried no behavior (no
// caller matches on the string; each just wraps a non-nil error as
// *ErrUnparseableOutput).
func ExtractJSONObject(s string) (map[string]any, error) {
	s = stripJSONFence(StripThinkTags(strings.TrimSpace(s)))
	var whole map[string]any
	if err := json.Unmarshal([]byte(s), &whole); err == nil {
		return whole, nil
	}
	var last map[string]any
	found := false
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var cand map[string]any
		if err := dec.Decode(&cand); err == nil {
			last = cand
			found = true
		}
	}
	if found {
		return last, nil
	}
	return nil, fmt.Errorf("agent: no JSON object found in result")
}

// stripJSONFence removes a single leading ```json / ``` fence line and a
// trailing ``` if present.
func stripJSONFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
