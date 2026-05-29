package droid

import (
	"fmt"
	"maps"
	"slices"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

var allowedKeys = map[string]struct{}{
	"prompt": {}, "model": {}, "reasoning_effort": {}, "autonomy": {},
	"system_prompt": {}, "enabled_tools": {}, "disabled_tools": {},
}

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

// ValidateConfig enforces droid's with-schema (same 5-step order as claude:
// session-reject → unknown-key → required-prompt → per-key types → API-key
// policy). Deterministic (sorted keys) — runs twice per node.
func (a *Adapter) ValidateConfig(with ir.RawConfig) error {
	for _, key := range sessionKeysList {
		if _, present := with[key]; present {
			return &ErrSessionReuseAttempted{Key: key}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(with)) {
		if _, ok := allowedKeys[k]; !ok {
			return &agent.ErrInvalidConfig{
				Ref: AdapterRef, Key: k,
				Reason: fmt.Sprintf("unknown with-key (allowed: %v)", slices.Sorted(maps.Keys(allowedKeys))),
			}
		}
	}
	prompt, ok := with["prompt"]
	if !ok {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "prompt", Reason: "required"}
	}
	ps, ok := prompt.(string)
	if !ok {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "prompt", Reason: fmt.Sprintf("must be string, got %T", prompt)}
	}
	if ps == "" {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "prompt", Reason: "must not be empty"}
	}
	if v, ok := with["model"]; ok {
		if _, ok := v.(string); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), "model")
		}
	}
	if v, ok := with["system_prompt"]; ok {
		if _, ok := v.(string); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), "system_prompt")
		}
	}
	if v, ok := with["reasoning_effort"]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), "reasoning_effort")
		}
		if !slices.Contains(reasoningEffortValues, s) {
			return wrapInvalidConfig(fmt.Sprintf("must be one of %v, got %q", reasoningEffortValues, s), "reasoning_effort")
		}
	}
	if v, ok := with["autonomy"]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), "autonomy")
		}
		if _, ok := autonomyFlags[s]; !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be one of %v, got %q", slices.Sorted(maps.Keys(autonomyFlags)), s), "autonomy")
		}
	}
	for _, key := range []string{"enabled_tools", "disabled_tools"} {
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
	if _, ok := a.env["FACTORY_API_KEY"]; !ok {
		return &ErrMissingAPIKey{AvailableKeys: slices.Sorted(maps.Keys(a.env))}
	}
	return nil
}
