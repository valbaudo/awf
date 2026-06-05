package droid

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/agent"
)

// This file owns droid's BYOK (bring-your-own-key) settings document — the
// `customModels` entry written to the container and passed to `droid exec
// --settings`. The command-assembly that consumes it (the printf prelude and the
// `custom:<model>` reference) lives in launch.go; the validation that gates it
// lives in validate.go (validateBYOK).

// droidSettingsPath is the in-container path the BYOK customModels settings file
// is written to (via printf in the same sh -c command) for `--settings`. Fixed
// path keeps Launch deterministic (no rand — CLAUDE.md); single-exec-per-container
// is guaranteed by the format (sequential gate/loop/retry; per-element map
// container). `>` truncates the file in the same command immediately before droid
// reads it, so any restored/leftover file is overwritten before use. Mirrors the
// codex schema-file prelude precedent.
const droidSettingsPath = "/tmp/awf-droid-settings.json"

// defaultProvider is droid's generic OpenAI-compatible provider — used when a BYOK
// step omits `provider`.
const defaultProvider = "generic-chat-completion-api"

// droidCustomModel is one entry in droid's customModels settings array. Field
// order is fixed (struct, not map) so the assembled command is deterministic.
// apiKey carries the LITERAL ${NAME} placeholder (never the resolved secret);
// droid expands it from its own process env at runtime. maxOutputTokens is OMITTED
// (droid defaults it).
type droidCustomModel struct {
	Model       string `json:"model"`
	DisplayName string `json:"displayName"`
	BaseURL     string `json:"baseUrl"`
	APIKey      string `json:"apiKey"`
	Provider    string `json:"provider"`
}

type droidSettings struct {
	CustomModels []droidCustomModel `json:"customModels"`
}

// byokSettingsJSON marshals the one-entry customModels settings document for a
// BYOK step. apiKey is the LITERAL "${<api_key_env>}" placeholder — the resolved
// secret value is NEVER read here, so it can never reach the command string;
// droid expands the placeholder from its own process env at runtime.
func byokSettingsJSON(inv agent.AgentInvocation) (string, error) {
	model, _ := inv.With[keyModel].(string)       // validateBYOK guarantees non-empty
	baseURL, _ := inv.With[keyBaseURL].(string)   // non-empty (BYOK branch)
	keyName, _ := inv.With[keyAPIKeyEnv].(string) // validateBYOK guarantees non-empty
	provider, _ := inv.With[keyProvider].(string)
	if provider == "" {
		provider = defaultProvider
	}
	doc := droidSettings{CustomModels: []droidCustomModel{{
		Model:       model,
		DisplayName: model,
		BaseURL:     baseURL,
		APIKey:      "${" + keyName + "}",
		Provider:    provider,
	}}}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("agent/droid: marshal BYOK settings: %w", err)
	}
	return string(b), nil
}
