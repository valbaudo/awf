package claude

import (
	"fmt"
	"maps"
	"slices"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// Per Phase 5 design decision 9. Strict; unknown keys rejected.
var allowedKeys = map[string]struct{}{
	"prompt":         {},
	"model":          {},
	"max_turns":      {},
	"system_prompt":  {},
	"allowed_tools":  {},
	"bare":           {},
	"max_budget_usd": {},
}

// sessionKeysList is the literal list of with-keys whose presence would
// re-use a claude session (Phase 5 design decision 7). Used only for the
// step-1 rejection loop in ValidateConfig.
var sessionKeysList = []string{"session_id", "continue", "resume"}

// ValidateConfig enforces the Claude Code adapter's with-schema. Returns
// *agent.ErrInvalidConfig for type / unknown-key violations, or one of the
// adapter-specific typed errors (*ErrSessionReuseAttempted,
// *ErrBareRequiresAPIKey) for the policy violations Phase 5 calls out.
//
// The errors.As-friendly typed errors let cli/run.go map each class to a
// clear operator-facing message.
func (a *Adapter) ValidateConfig(with ir.RawConfig) error {
	// 1. Session-flag rejection FIRST so a workflow with session_id ALSO
	// missing prompt gets the more specific error.
	for _, key := range sessionKeysList {
		if _, present := with[key]; present {
			return &ErrSessionReuseAttempted{Key: key}
		}
	}

	// 2. Unknown-key rejection. Walk the map in deterministic order for
	// reproducible error messages.
	for _, k := range slices.Sorted(maps.Keys(with)) {
		if _, ok := allowedKeys[k]; !ok {
			return &agent.ErrInvalidConfig{
				Ref:    AdapterRef,
				Key:    k,
				Reason: fmt.Sprintf("unknown with-key (allowed: %v)", slices.Sorted(maps.Keys(allowedKeys))),
			}
		}
	}

	// 3. prompt (required, string).
	prompt, ok := with["prompt"]
	if !ok {
		return &agent.ErrInvalidConfig{
			Ref:    AdapterRef,
			Key:    "prompt",
			Reason: "required",
		}
	}
	if _, ok := prompt.(string); !ok {
		return &agent.ErrInvalidConfig{
			Ref:    AdapterRef,
			Key:    "prompt",
			Reason: fmt.Sprintf("must be string, got %T", prompt),
		}
	}

	// 4. Optional-field types (each present-key must be the right Go type).
	if v, ok := with["model"]; ok {
		if _, ok := v.(string); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), "model")
		}
	}
	if v, ok := with["max_turns"]; ok {
		switch v.(type) {
		case int, int64, float64: // YAML decodes ints sometimes as float64
		default:
			return wrapInvalidConfig(fmt.Sprintf("must be integer, got %T", v), "max_turns")
		}
	}
	if v, ok := with["system_prompt"]; ok {
		if _, ok := v.(string); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), "system_prompt")
		}
	}
	if v, ok := with["allowed_tools"]; ok {
		if _, ok := v.([]any); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be array of strings, got %T", v), "allowed_tools")
		}
	}
	if v, ok := with["bare"]; ok {
		if _, ok := v.(bool); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be bool, got %T", v), "bare")
		}
	}
	if v, ok := with["max_budget_usd"]; ok {
		switch v.(type) {
		case int, int64, float64:
		default:
			return wrapInvalidConfig(fmt.Sprintf("must be number, got %T", v), "max_budget_usd")
		}
	}

	// 5. bare-requires-API-key. AWF default for bare is true (decision 9).
	bare := true
	if v, ok := with["bare"]; ok {
		bare = v.(bool)
	}
	if bare {
		_, haveAPIKey := a.env["ANTHROPIC_API_KEY"]
		_, haveAuthToken := a.env["ANTHROPIC_AUTH_TOKEN"]
		if !haveAPIKey && !haveAuthToken {
			return &ErrBareRequiresAPIKey{AvailableKeys: slices.Sorted(maps.Keys(a.env))}
		}
	}

	return nil
}
