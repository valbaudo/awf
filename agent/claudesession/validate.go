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
	// keyWorkdir is the in-container working directory for this step. The
	// session transcript path is keyed on it
	// (~/.claude/projects/<encodeProjectDir(workdir)>/…). Authors MUST set
	// this to the directory claude will run in (the image's WORKDIR, or a
	// compose-declared working_dir). When absent the transcript path uses ""
	// as the project bucket, which is degenerate and will NOT match where
	// claude writes; the field is optional only to preserve backward
	// compatibility with existing conformance tests that use a fixed-path
	// fake adapter.
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
