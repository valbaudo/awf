// Package claudesession implements agent.Adapter for Claude Code with
// deterministic session-id reuse (anthropic/claude-code-session). It is
// the PersistentSession counterpart to agent/claude (which is the
// fresh-per-launch, gate-safe adapter).
//
// Design decisions locked in the M2 task brief (2026-06-25):
//   - Session UUID is deterministic: sha256(runID|epoch|nodePath), first 16
//     bytes formatted as a UUID v4 string. Same inputs → same UUID; different
//     nodePath → different UUID.
//   - Transcript path follows Claude Code's project-journal layout:
//     <home>/.claude/projects/<encodeProjectDir(workdir)>/<uuid>.jsonl
//   - Reuses the base claude.Adapter for all shared logic (env, backend,
//     version, stream parsing). Launch appends --session-id <uuid> to the
//     assembled command, and drops --no-session-persistence so the host's
//     ~/.claude/projects/ journal records the AWF session.
//   - Capabilities(): NativeSchema:true, PersistentSession:true.
//   - The adapter is a container-backed CLI adapter (NOT Containerless).
//
// The exact in-container HOME is NOT yet finalised — that is resolved in the
// live integration task. The HomeDir option defaults to "/root" (the common
// Claude Code container image default); callers may override it.
package claudesession

import (
	"fmt"

	"github.com/valbaudo/awf/agent"
)

// AdapterRef is the agent-runtime identifier this package's Adapter
// returns from Ref(). Constant so cli/agent_registry.go and unit tests
// can refer to it without typing the string literal.
const AdapterRef = "anthropic/claude-code-session"

// DefaultHomeDir is the in-container home directory used to derive the
// transcript path. The exact value is finalised in the live integration task
// (task M2d); containers running Claude Code typically run as root.
const DefaultHomeDir = "/root"

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
