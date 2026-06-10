package codexlive

// DefaultEnvAllowlist mirrors the strict Codex adapter's auth/config inputs.
// CODEX_HOME preserves local Codex login/cache context; OPENAI_API_KEY covers
// API-key based Codex installs.
var DefaultEnvAllowlist = []string{"OPENAI_API_KEY", "CODEX_HOME"}
