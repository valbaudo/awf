package goose

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// with-config key names. The schema's canonical keys, shared by ValidateConfig
// (allowedKeys + per-key checks) and assembleCommand (launch.go) so the two can
// never disagree on a key name and silently drop a configured value.
const (
	keyPrompt       = "prompt"
	keyModel        = "model"
	keyMaxTurns     = "max_turns"
	keySystemPrompt = "system_prompt"
)

var allowedKeys = map[string]struct{}{
	keyPrompt: {}, keyModel: {}, keyMaxTurns: {}, keySystemPrompt: {},
}

// sessionKeysList — with-keys that would reuse/continue a prior goose session,
// breaking gate independence. The first six are goose's real reuse flags;
// `continue` is a defensive entry (no real goose flag) matching droid/claude.
var sessionKeysList = []string{"resume", "name", "session_id", "path", "fork", "interactive", "continue"}

// providerAuthKey maps a GOOSE_PROVIDER value to the env key it requires. Absence =
// no key (claude-code delegates to the claude binary; unknown providers ungated).
var providerAuthKey = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"openai":    "OPENAI_API_KEY",
}

// ValidateConfig enforces goose's with-schema (droid's 5-step order: session-reject
// → unknown-key → required-prompt → per-key types → provider-conditional auth).
// Deterministic (sorted keys); runs twice per node.
func (a *Adapter) ValidateConfig(with ir.RawConfig) error {
	for _, key := range sessionKeysList {
		if _, present := with[key]; present {
			return &ErrSessionReuseAttempted{Key: key}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(with)) {
		if _, ok := allowedKeys[k]; !ok {
			return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: k, Reason: fmt.Sprintf("unknown with-key (allowed: %v)", slices.Sorted(maps.Keys(allowedKeys)))}
		}
	}
	prompt, ok := with[keyPrompt]
	if !ok {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: keyPrompt, Reason: "required"}
	}
	ps, ok := prompt.(string)
	if !ok {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: keyPrompt, Reason: fmt.Sprintf("must be string, got %T", prompt)}
	}
	if ps == "" {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: keyPrompt, Reason: "must not be empty"}
	}
	for _, key := range []string{keyModel, keySystemPrompt} {
		if v, ok := with[key]; ok {
			if _, ok := v.(string); !ok {
				return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), key)
			}
		}
	}
	if v, ok := with[keyMaxTurns]; ok {
		n, ok := asInt(v)
		if !ok || n <= 0 {
			return wrapInvalidConfig(fmt.Sprintf("must be a positive integer, got %v (%T)", v, v), keyMaxTurns)
		}
	}
	// Provider-conditional auth (defense-in-depth): only enforce when GOOSE_PROVIDER
	// is set in the adapter env (then goose obeys env > config.yaml, so validated ==
	// runtime). When unset, the active provider lives only in config.yaml, which the
	// adapter cannot see → skip; a missing key still fails LOUD at Launch (goose exits
	// 1 with "error: Error Configuration value not found: <KEY>" → permanent).
	if provider, ok := a.env["GOOSE_PROVIDER"]; ok {
		if key, needs := providerAuthKey[provider]; needs {
			if _, present := a.env[key]; !present {
				return &ErrMissingAPIKey{Key: key}
			}
		}
	}
	return nil
}

// asInt accepts the numeric shapes ir.RawConfig may carry for an integer with-key
// (int from a Go literal, float64 from JSON, json.Number from a streaming decoder).
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}
