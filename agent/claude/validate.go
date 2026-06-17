package claude

import (
	"fmt"
	"maps"
	"slices"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// with-config key names. Shared by ValidateConfig (allowedKeys + per-key checks)
// and assembleCommand (launch.go) so the two can never disagree on a key name —
// mirrors agent/codex/validate.go.
const (
	keyPrompt       = "prompt"
	keyModel        = "model"
	keyEffort       = "effort"
	keyMaxTurns     = "max_turns"
	keySystemPrompt = "system_prompt"
	keyAllowedTools = "allowed_tools"
	keyBare         = "bare"
	keyMaxBudgetUSD = "max_budget_usd"
)

// defaultBare is the AWF default for the `bare` key (Phase 5 decision 9): true.
// One source read by BOTH the validate-time API-key gate and the launch-time
// `--bare` flag emission.
const defaultBare = true

// Per Phase 5 design decision 9. Strict; unknown keys rejected.
var allowedKeys = map[string]struct{}{
	keyPrompt: {}, keyModel: {}, keyEffort: {}, keyMaxTurns: {}, keySystemPrompt: {},
	keyAllowedTools: {}, keyBare: {}, keyMaxBudgetUSD: {},
}

var effortValues = []string{"low", "medium", "high", "xhigh", "max"}

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
	prompt, ok := with[keyPrompt]
	if !ok {
		return &agent.ErrInvalidConfig{
			Ref:    AdapterRef,
			Key:    keyPrompt,
			Reason: "required",
		}
	}
	if _, ok := prompt.(string); !ok {
		return &agent.ErrInvalidConfig{
			Ref:    AdapterRef,
			Key:    keyPrompt,
			Reason: fmt.Sprintf("must be string, got %T", prompt),
		}
	}

	// 4. Optional-field types (each present-key must be the right Go type).
	if v, ok := with[keyModel]; ok {
		if _, ok := v.(string); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyModel)
		}
	}
	if v, ok := with[keyEffort]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyEffort)
		}
		if !slices.Contains(effortValues, s) {
			return wrapInvalidConfig(fmt.Sprintf("must be one of %v, got %q", effortValues, s), keyEffort)
		}
	}
	if v, ok := with[keyMaxTurns]; ok {
		switch v.(type) {
		case int, int64, float64: // YAML decodes ints sometimes as float64
		default:
			return wrapInvalidConfig(fmt.Sprintf("must be integer, got %T", v), keyMaxTurns)
		}
	}
	if v, ok := with[keySystemPrompt]; ok {
		if _, ok := v.(string); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keySystemPrompt)
		}
	}
	if v, ok := with[keyAllowedTools]; ok {
		if _, ok := v.([]any); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be array of strings, got %T", v), keyAllowedTools)
		}
	}
	if v, ok := with[keyBare]; ok {
		if _, ok := v.(bool); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be bool, got %T", v), keyBare)
		}
	}
	if v, ok := with[keyMaxBudgetUSD]; ok {
		switch v.(type) {
		case int, int64, float64:
		default:
			return wrapInvalidConfig(fmt.Sprintf("must be number, got %T", v), keyMaxBudgetUSD)
		}
	}

	// 5. bare-requires-API-key. AWF default for bare is true (decision 9).
	bare := defaultBare
	if v, ok := with[keyBare]; ok {
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
