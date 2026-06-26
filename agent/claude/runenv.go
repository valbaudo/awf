package claude

import "github.com/valbaudo/awf/agent"

// ApplyPerRunConfigEnv sets the claude per-run config-isolation environment on env,
// which MUST be a fresh, caller-owned map (never the adapter's shared a.env). It
// always disables non-essential traffic (headless hygiene). When the engine has
// computed a per-run config dir (inv.SessionConfigDir), it points claude at it AND
// relocates claude's XDG state+cache there.
//
// The XDG redirect is load-bearing for NATIVE concurrency. claude-code keeps a
// per-version single-instance lock at $XDG_STATE_HOME/claude/locks/<version>.lock —
// OUTSIDE CLAUDE_CONFIG_DIR — plus a cache under $XDG_CACHE_HOME/claude. On the
// native backend every concurrent run inherits the shared host $HOME (hence the
// shared $XDG_STATE_HOME), so with only a per-run CLAUDE_CONFIG_DIR two concurrent
// runs of the same claude version still contend on the SAME lock and the loser
// fails ("1 concurrent OK, 2+ fail"). Pointing XDG_STATE_HOME/XDG_CACHE_HOME at
// per-run subdirs of the (sandbox-writable) config dir isolates that lock and cache
// per run. The subdirs sit alongside — not under — CLAUDE_CONFIG_DIR/projects, so
// the session-transcript subtree capture is unaffected.
//
// HOME and XDG_DATA_HOME are deliberately NOT set: claude's versioned binary lives
// under $XDG_DATA_HOME (~/.local/share/claude/versions/<v>), which must stay shared
// and resolvable — relocating it would break binary resolution.
//
// KEEP IN SYNC: agent/claude.Launch and agent/claudesession.Launch both call this.
func ApplyPerRunConfigEnv(env map[string]string, inv agent.AgentInvocation) {
	env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	if inv.SessionConfigDir != "" {
		env["CLAUDE_CONFIG_DIR"] = inv.SessionConfigDir
		env["XDG_STATE_HOME"] = inv.SessionConfigDir + "/xdg-state"
		env["XDG_CACHE_HOME"] = inv.SessionConfigDir + "/xdg-cache"
	}
}
