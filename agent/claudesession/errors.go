// Package claudesession implements agent.Adapter for Claude Code with
// deterministic session-id reuse (anthropic/claude-code-session). It is
// the PersistentSession counterpart to agent/claude (which is the
// fresh-per-launch, gate-safe adapter).
//
// Design (M2 + the 2026-06-26 native-config-isolation revision):
//   - Session UUID is deterministic: sha256(runID|epoch|nodePath), first 16
//     bytes formatted as a UUID string. Same inputs → same UUID; different
//     nodePath → different UUID. It is passed to claude's --session-id / --resume.
//   - Reuses the base claude.Adapter for all shared logic (env, backend,
//     version, stream parsing). Launch appends --session-id <uuid> and drops
//     --no-session-persistence so claude records the session journal.
//   - Per-run isolation: the engine computes a per-run CLAUDE_CONFIG_DIR
//     (<staging-root>/claude-session/<run-id>) and threads it via
//     inv.SessionConfigDir; Launch sets it on the exec env so claude relocates
//     its whole config tree per run. The engine captures/restores the
//     <CLAUDE_CONFIG_DIR>/projects subtree as the SessionRef (no path derivation
//     here — the adapter no longer computes a transcript path).
//   - Capabilities(): NativeSchema:true, PersistentSession:true.
//   - The adapter is a container-backed CLI adapter (NOT Containerless) — subtree
//     capture requires a container filesystem.
package claudesession

import (
	"fmt"

	"github.com/valbaudo/awf/agent"
)

// AdapterRef is the agent-runtime identifier this package's Adapter
// returns from Ref(). Constant so cli/agent_registry.go and unit tests
// can refer to it without typing the string literal.
const AdapterRef = "anthropic/claude-code-session"

// DefaultEnvAllowlist is the same credential set as agent/claude since
// claude-code-session launches the same `claude -p` binary with the same
// auth environment. Single source of truth for cli/agent_registry.go's
// adapterEnvAllowlists entry and the integ tests' skipIfNoAuthEnv helper.
var DefaultEnvAllowlist = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN",
}

// ErrBareRequiresAPIKey is returned by ValidateConfig when `with.bare` is
// true but neither ANTHROPIC_API_KEY nor ANTHROPIC_AUTH_TOKEN is present in
// the adapter's env allowlist. Mirrors the error from agent/claude for this
// adapter's Ref.
type ErrBareRequiresAPIKey struct {
	AvailableKeys []string
}

func (e *ErrBareRequiresAPIKey) Error() string {
	if len(e.AvailableKeys) == 0 {
		return "agent/claudesession: with.bare: true requires ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN in the adapter's env allowlist (--agent-env), but the allowlist is empty"
	}
	return fmt.Sprintf("agent/claudesession: with.bare: true requires ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN in the adapter's env allowlist (--agent-env); allowlist has %v but neither key is present", e.AvailableKeys)
}

// wrapInvalidConfig wraps an agent.ErrInvalidConfig for this adapter.
func wrapInvalidConfig(reason string, key string) error {
	return &agent.ErrInvalidConfig{
		Ref:    AdapterRef,
		Key:    key,
		Reason: reason,
	}
}
