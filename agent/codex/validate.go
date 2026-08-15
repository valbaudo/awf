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
	keyPrompt  = "prompt"
	keyModel   = "model"
	keySandbox = "sandbox"
	keyEffort  = "effort"
)

var allowedKeys = map[string]struct{}{
	keyPrompt: {}, keyModel: {}, keySandbox: {}, keyEffort: {},
}

// renamedKeys — with-keys that used to exist under a different name. F12: the
// codex/codexlive/droid adapters all called this key `reasoning_effort`;
// `effort` is now the canonical name (matching anthropic/claude-code). Checked
// BEFORE the generic unknown-key loop so the author gets a specific rename
// pointer instead of a bare "unknown with-key" message. KeyUnknown: true so
// the run-start with:-config guard (U1) surfaces this pre-spend, same as any
// other unknown key.
var renamedKeys = map[string]string{"reasoning_effort": "effort"}

// sessionKeysList — with-keys that would reuse/continue a prior codex session.
// `last`/`session_id` are resume/fork SUBCOMMAND args (not `codex exec` flags);
// `continue` is defensive (no real codex flag). Defense-in-depth: the adapter
// always runs bare `codex exec`, never a subcommand.
var sessionKeysList = []string{"resume", "fork", "last", "session_id", "continue"}

// sandboxValues — codex exec --sandbox accepted modes.
var sandboxValues = []string{"read-only", "workspace-write", "danger-full-access"}

// Effort validation (below, in ValidateConfig) checks TRANSPORT safety only
// (agent.IsBareWord): the value reaches codex as a bare TOML value in
// -c model_reasoning_effort=<value> on a shell-quoted command line. codex
// owns the tier vocabulary — v0.131.0 topped out at xhigh, v0.146.0 added
// max/ultra, and the enum this replaced (verified-then-frozen against
// v0.131.0) rejected those VALID tiers at validate time (2026-08-15).
// A bogus tier fails loudly at the CLI/API at run time instead.

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
	for _, old := range slices.Sorted(maps.Keys(renamedKeys)) {
		if _, ok := with[old]; ok {
			return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: old, Reason: "renamed to " + renamedKeys[old], KeyUnknown: true}
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
	if v, ok := with[keyEffort]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyEffort)
		}
		if !agent.IsBareWord(s) {
			return wrapInvalidConfig(fmt.Sprintf("must be a bare word (lowercase letters only; it is interpolated as a bare TOML value), got %q", s), keyEffort)
		}
	}
	return nil
}
