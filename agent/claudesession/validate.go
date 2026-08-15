package claudesession

// with-config key names. Mirror agent/claude's key set exactly — the same
// with: schema applies to both adapters (anthropic/claude-code and
// anthropic/claude-code-session share the same CLI flags), plus workdir.
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

// defaultBare mirrors the base claude adapter's default (Phase 5 decision 9).
const defaultBare = true

// allowedKeys is the strict allowed set for ValidateConfig.
var allowedKeys = map[string]struct{}{
	keyPrompt: {}, keyModel: {}, keyEffort: {}, keyMaxTurns: {}, keySystemPrompt: {},
	keyAllowedTools: {}, keyBare: {}, keyMaxBudgetUSD: {},
}

// (effort enum removed 2026-08-15 — transport-checked via agent.IsBareWord at
// the check site in adapter.go, same as the base claude adapter)
