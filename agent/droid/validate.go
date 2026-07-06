package droid

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// with-config key names — the single source of truth for droid's `with:` schema,
// shared by ValidateConfig (this file) and assembleCommand/byokSettingsJSON
// (launch.go, byok.go) so the validator and the command builder can never
// disagree about a key string. The BYOK names (base_url/api_key_env/tls_insecure)
// match agent/awfllm for cross-adapter consistency.
const (
	keyPrompt        = "prompt"
	keyModel         = "model"
	keyEffort        = "effort"
	keyAutonomy      = "autonomy"
	keySystemPrompt  = "system_prompt"
	keyEnabledTools  = "enabled_tools"
	keyDisabledTools = "disabled_tools"
	keyBaseURL       = "base_url"
	keyAPIKeyEnv     = "api_key_env"
	keyProvider      = "provider"
	keyTLSInsecure   = "tls_insecure"
)

var allowedKeys = map[string]struct{}{
	keyPrompt: {}, keyModel: {}, keyEffort: {}, keyAutonomy: {},
	keySystemPrompt: {}, keyEnabledTools: {}, keyDisabledTools: {},
	// BYOK (custom OpenAI-compatible endpoint). `base_url` set ⇒ BYOK mode;
	// see the BYOK branch in ValidateConfig.
	keyBaseURL: {}, keyAPIKeyEnv: {}, keyProvider: {}, keyTLSInsecure: {},
}

// rejectedKeys never belong in `with:` — `api_key` would inline a secret into
// the durable definition; name the host env var with `api_key_env` instead.
var rejectedKeys = []string{"api_key"}

// renamedKeys — with-keys that used to exist under a different name. F12:
// `effort` is now the canonical name (matching anthropic/claude-code and
// openai/codex); `reasoning_effort` no longer exists. Checked BEFORE the
// generic unknown-key loop so the author gets a specific rename pointer.
// KeyUnknown: true so the run-start with:-config guard (U1) surfaces this
// pre-spend, same as any other unknown key.
var renamedKeys = map[string]string{"reasoning_effort": "effort"}

// sessionKeysList — with-keys that would reuse/continue a prior droid session,
// breaking gate independence. Note `fork` is droid-specific (the claude sibling
// has no fork flag); do not "align" this list to claude's.
var sessionKeysList = []string{"session_id", "resume", "fork", "continue"}

// reasoningEffortValues is droid's accepted --reasoning-effort superset. droid
// enforces a per-model subset at exec; a model-invalid value passes here but is
// caught as a permanent config error from stderr at Launch (see launch.go).
var reasoningEffortValues = []string{"off", "none", "minimal", "low", "medium", "high", "xhigh", "max"}

// wrapInvalidConfig builds the engine-classified *agent.ErrInvalidConfig.
func wrapInvalidConfig(reason string, key string) error {
	return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: reason}
}

// ValidateConfig enforces droid's with-schema (same deterministic order as
// claude: reject-inline/session → unknown-key → required-prompt → per-key types
// → BYOK-or-API-key policy). Deterministic (sorted keys) — runs twice per node.
func (a *Adapter) ValidateConfig(with ir.RawConfig) error {
	for _, key := range sessionKeysList {
		if _, present := with[key]; present {
			return &ErrSessionReuseAttempted{Key: key}
		}
	}
	for _, k := range rejectedKeys {
		if _, present := with[k]; present {
			return wrapInvalidConfig("not supported (use api_key_env to name a host env var)", k)
		}
	}
	for old, newKey := range renamedKeys {
		if _, ok := with[old]; ok {
			return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: old, Reason: "renamed to " + newKey, KeyUnknown: true}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(with)) {
		if _, ok := allowedKeys[k]; !ok {
			return &agent.ErrInvalidConfig{
				Ref: AdapterRef, Key: k,
				Reason:     fmt.Sprintf("unknown with-key (allowed: %v)", slices.Sorted(maps.Keys(allowedKeys))),
				KeyUnknown: true,
			}
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
	if v, ok := with[keyModel]; ok {
		if _, ok := v.(string); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyModel)
		}
	}
	if v, ok := with[keySystemPrompt]; ok {
		if _, ok := v.(string); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keySystemPrompt)
		}
	}
	if v, ok := with[keyEffort]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyEffort)
		}
		if !slices.Contains(reasoningEffortValues, s) {
			return wrapInvalidConfig(fmt.Sprintf("must be one of %v, got %q", reasoningEffortValues, s), keyEffort)
		}
	}
	if v, ok := with[keyAutonomy]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyAutonomy)
		}
		if _, ok := autonomyFlags[s]; !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be one of %v, got %q", slices.Sorted(maps.Keys(autonomyFlags)), s), keyAutonomy)
		}
	}
	for _, key := range []string{keyEnabledTools, keyDisabledTools} {
		if v, ok := with[key]; ok {
			arr, ok := v.([]any)
			if !ok {
				return wrapInvalidConfig(fmt.Sprintf("must be array of strings, got %T", v), key)
			}
			for i, elem := range arr {
				if _, ok := elem.(string); !ok {
					return wrapInvalidConfig(fmt.Sprintf("element %d must be string, got %T", i, elem), key)
				}
			}
		}
	}
	for _, key := range []string{keyBaseURL, keyAPIKeyEnv, keyProvider} {
		if v, ok := with[key]; ok {
			if _, ok := v.(string); !ok {
				return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), key)
			}
		}
	}
	if v, ok := with[keyTLSInsecure]; ok {
		if _, ok := v.(bool); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be a bool, got %T", v), keyTLSInsecure)
		}
	}
	// BYOK mode is triggered by a non-empty base_url (custom OpenAI-compatible
	// endpoint). Pure BYOK needs no Factory account, so the FACTORY_API_KEY
	// requirement is dropped here — the named api_key_env var carries auth.
	if baseURL, _ := with[keyBaseURL].(string); baseURL != "" {
		return a.validateBYOK(with)
	}
	if _, ok := a.env["FACTORY_API_KEY"]; !ok {
		return &ErrMissingAPIKey{AvailableKeys: slices.Sorted(maps.Keys(a.env))}
	}
	return nil
}

// validateBYOK enforces the extra rules that only apply when base_url is set:
// model must be a non-empty whitespace-free string (it becomes a `custom:<model>`
// CLI ref and a gateway model name) and api_key_env must name a host env var
// present in the forwarded allowlist (a.env). Type checks already ran above.
func (a *Adapter) validateBYOK(with ir.RawConfig) error {
	model, ok := with[keyModel].(string)
	if !ok || model == "" {
		return wrapInvalidConfig("required (a non-empty model is the custom:<model> reference) when base_url is set", keyModel)
	}
	if strings.ContainsFunc(model, unicode.IsSpace) {
		return wrapInvalidConfig(fmt.Sprintf("must not contain whitespace when base_url is set, got %q", model), keyModel)
	}
	keyName, ok := with[keyAPIKeyEnv].(string)
	if !ok || keyName == "" {
		return wrapInvalidConfig("required (names the host env var holding the API key) when base_url is set", keyAPIKeyEnv)
	}
	if _, present := a.env[keyName]; !present {
		return wrapInvalidConfig(fmt.Sprintf("env var %q not present in the forwarded allowlist (available: %v)", keyName, slices.Sorted(maps.Keys(a.env))), keyAPIKeyEnv)
	}
	return nil
}
