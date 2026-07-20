package native

// sandbox.go — OS-agnostic sandbox seam for the native backend.
//
// Architecture (WS-5 Task 2):
//
//   - sandboxLauncher: the per-OS launcher interface.
//   - detectSandbox:   calls detectPlatformSandbox (build-tagged per OS), then
//     falls back to noOpLauncher + emits a loud no-isolation warning label.
//   - credDirs:        enumerates agent config dirs for later RO-mount/allow by
//     the real launchers (T3/T4); returned as a deduplicated []string.
//   - noOpLauncher:    always returns nil (no argv prefix — command runs unconstrained).
//
// Per-OS bodies live in:
//   - sandbox_linux.go  (linux build tag) — T3 fills the bwrap→landlock chain.
//   - sandbox_darwin.go (darwin build tag) — T4 fills sandbox-exec.
//   - sandbox_other.go  (!linux && !darwin) — permanent stub, no-op forever.
//
// detectSandbox takes a lookPath seam. Platform selectors may perform further
// non-mutating capability probes: Linux functionally probes bwrap and checks
// the Landlock ABI before returning a launcher.

import (
	"os"
	"path/filepath"
)

// sandboxLauncher is implemented by each platform's sandbox strategy.
// prepend returns the argv prefix to prepend to ["sh", "-c", run], or nil
// (the no-op case: command runs on the host without any confinement wrapper).
//
// T3/T4 contract: prepend must be a pure function of its arguments (no global
// state mutations); it must not exec anything itself.
type sandboxLauncher interface {
	prepend(scratchDir string, rwDirs, roDirs []string) []string
}

// detectSandbox returns the best available sandboxLauncher for the current
// host, plus a human-readable label string. If no platform sandbox is
// available, it returns the noOpLauncher and a loud no-isolation warning
// label (callers should surface this via the same stderr warning path as
// cli/run.go:354 — see Backend.New + WithSandbox).
//
// lookPath is injectable for cross-platform unit tests; Linux's full selector
// has additional injected probe seams in sandbox_linux.go.
func detectSandbox(lookPath func(string) (string, error)) (sandboxLauncher, string) {
	if l, label := detectPlatformSandbox(lookPath); l != nil {
		return l, label
	}
	// No platform sandbox available — fall back to no-op + loud warn label.
	return noOpLauncher{}, noSandboxWarnLabel
}

// noSandboxWarnLabel is the human-readable label emitted when the native
// backend runs without any sandbox. Kept here (not in cli/) so the Backend
// can surface it on the same stderr path as the existing no-image warning.
const noSandboxWarnLabel = "no-sandbox (WARNING: steps run on host without OS-level isolation)"

// sandboxLauncherFactory is implemented by per-OS launcher types that need
// the run-command string at dispatch time (bubblewrap, landlock-trampoline).
// exec.go type-asserts b.sandbox to sandboxLauncherFactory to get a
// per-dispatch sandboxLauncher with the run string baked in.
//
// Motivation: the sandboxLauncher.prepend interface is a pure function of
// (scratchDir, roDirs) — it cannot carry per-dispatch state like the shell
// command to run. Factories bridge the gap: constructed at detect-time with
// the binary path and host config, they produce a fresh sandboxLauncher per
// dispatch with the run command embedded.
//
// noOpLauncher does NOT implement this interface — it returns nil from prepend
// unconditionally, which exec.go interprets as "run sh directly."
type sandboxLauncherFactory interface {
	buildForRun(run string) sandboxLauncher
}

// noOpLauncher is the fallback sandboxLauncher. It signals "no confinement"
// by returning nil from prepend, which the caller interprets as "no argv
// prefix — run sh directly."
type noOpLauncher struct{}

func (noOpLauncher) prepend(_ string, _, _ []string) []string { return nil }

// credDirs returns the deduplicated list of agent credential / config
// directories that OS-specific launchers should mount read-only (or add to
// their allow-list). The per-run HOME is passed in as runHome; the function
// resolves all other dirs relative to it or from env vars.
//
// Included dirs (in order, deduped):
//   - runHome/.claude      (Claude Code session config)
//   - $CODEX_HOME          (if set), else runHome/.codex
//   - runHome/.factory
//   - runHome/.config/goose
//   - runHome/.config
//
// Entries that cannot be resolved are silently skipped; callers receive only
// paths that could in principle exist.
func credDirs(runHome string) []string {
	return dedupeAbsDirs(append(agentConfigDirs(runHome), filepath.Join(runHome, ".config")))
}

// credDirsWritable returns the subset of credential dirs that must be mounted
// READ-WRITE so an agent's token refresh persists across runs instead of
// vanishing when the sandbox tears down its mount namespace. Verified on Linux
// (no keyring, files are the store): droid refreshes to ~/.factory/auth.v2.file,
// codex to ~/.codex/auth.json — each INSIDE its own enumerated dir. It
// deliberately EXCLUDES the bare ~/.config catch-all (returned RO by credDirs)
// so a step never gains write to the whole XDG tree (git, gh, shell config).
//
// ponytail: a fixed dir set, not a per-adapter Caps channel — every
// file-refreshing agent writes inside its own config dir, so the running
// adapter's identity is not needed to decide this. macOS stores these creds in
// the keychain (survives the HOME tmpfs), so the darwin launcher ignores this
// list; the write-loss bug is Linux-only.
func credDirsWritable(runHome string) []string {
	return dedupeAbsDirs(agentConfigDirs(runHome))
}

// agentConfigDirs are the per-agent config/credential dirs (Claude, Codex,
// Factory/droid, Goose). $CODEX_HOME overrides ~/.codex.
func agentConfigDirs(runHome string) []string {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(runHome, ".codex")
	}
	return []string{
		filepath.Join(runHome, ".claude"),
		codexHome,
		filepath.Join(runHome, ".factory"),
		filepath.Join(runHome, ".config", "goose"),
	}
}

// dedupeAbsDirs drops empty, relative, and duplicate paths (order preserved).
// Relative paths are dropped because filepath.Join("", ".claude") yields the
// relative ".claude", which could resolve to an unintended host path when used
// as a bind mount or Landlock rule.
func dedupeAbsDirs(candidates []string) []string {
	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, d := range candidates {
		if d == "" || !filepath.IsAbs(d) || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}
