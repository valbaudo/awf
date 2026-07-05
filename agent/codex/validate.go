package codex

import (
	"fmt"
	"maps"
	"slices"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// with-config key names. Shared by ValidateConfig (allowedKeys + per-key checks)
// and assembleCommand (launch.go) so the two can never disagree on a key name.
const (
	keyPrompt          = "prompt"
	keyModel           = "model"
	keySandbox         = "sandbox"
	keyReasoningEffort = "reasoning_effort"
)

var allowedKeys = map[string]struct{}{
	keyPrompt: {}, keyModel: {}, keySandbox: {}, keyReasoningEffort: {},
}

// sessionKeysList — with-keys that would reuse/continue a prior codex session.
// `last`/`session_id` are resume/fork SUBCOMMAND args (not `codex exec` flags);
// `continue` is defensive (no real codex flag). Defense-in-depth: the adapter
// always runs bare `codex exec`, never a subcommand.
var sessionKeysList = []string{"resume", "fork", "last", "session_id", "continue"}

// sandboxValues — codex exec --sandbox accepted modes.
var sandboxValues = []string{"read-only", "workspace-write", "danger-full-access"}

// reasoningEffortValues — codex's accepted model_reasoning_effort tiers, VERIFIED
// against the v0.131.0 binary's own config validation: a bad value yields
// "unknown variant ..., expected one of none, minimal, low, medium, high, xhigh"
// at config-load, before any API call. Enum-validated here so a bad value fails at
// awf-validate time, and only a bare-word, TOML-safe value reaches the
// -c model_reasoning_effort= flag. (codex also rejects a bad value loudly at
// config-load — this is fail-fast on top of that.)
var reasoningEffortValues = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// ValidateConfig enforces codex's with-schema (session-reject → unknown-key →
// required-prompt → per-key types+enums). Deterministic (sorted keys); runs twice
// per node (run-start walk + defensive dispatch) → idempotent. No
// provider-conditional auth gate: codex auth (ChatGPT-OAuth or OPENAI_API_KEY)
// cannot be statically probed — a missing credential surfaces as a loud
// turn.failed at Launch.
func (a *Adapter) ValidateConfig(with ir.RawConfig) error {
	for _, key := range sessionKeysList {
		if _, present := with[key]; present {
			return &ErrSessionReuseAttempted{Key: key}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(with)) {
		if _, ok := allowedKeys[k]; !ok {
			return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: k, Reason: fmt.Sprintf("unknown with-key (allowed: %v)", slices.Sorted(maps.Keys(allowedKeys))), KeyUnknown: true}
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
	if v, ok := with[keySandbox]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keySandbox)
		}
		if !slices.Contains(sandboxValues, s) {
			return wrapInvalidConfig(fmt.Sprintf("must be one of %v, got %q", sandboxValues, s), keySandbox)
		}
	}
	if v, ok := with[keyReasoningEffort]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyReasoningEffort)
		}
		if !slices.Contains(reasoningEffortValues, s) {
			return wrapInvalidConfig(fmt.Sprintf("must be one of %v, got %q", reasoningEffortValues, s), keyReasoningEffort)
		}
	}
	return nil
}
