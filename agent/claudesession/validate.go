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
	// keyWorkdir was the in-container working directory used by the OLD
	// single-transcript-file session model to derive the ~/.claude/projects
	// bucket. The 2026-06-26 native-config-isolation revision captures the whole
	// per-run CLAUDE_CONFIG_DIR/projects subtree instead, so the transcript bucket
	// lives INSIDE the captured artifact and this key no longer participates in
	// the session path. It is still ACCEPTED (optional) for backward
	// compatibility; dropping it is a tracked follow-up (needs a man-page update).
	keyWorkdir = "workdir"
)

// defaultBare mirrors the base claude adapter's default (Phase 5 decision 9).
const defaultBare = true

// allowedKeys is the strict allowed set for ValidateConfig.
var allowedKeys = map[string]struct{}{
	keyPrompt: {}, keyModel: {}, keyEffort: {}, keyMaxTurns: {}, keySystemPrompt: {},
	keyAllowedTools: {}, keyBare: {}, keyMaxBudgetUSD: {}, keyWorkdir: {},
}

var effortValues = []string{"low", "medium", "high", "xhigh", "max"}
