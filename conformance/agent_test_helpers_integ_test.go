//go:build integ && live

package conformance

import (
	"os"
	"os/exec"
	"testing"

	"github.com/valbaudo/awf/agent/claude"
)

// envFromHost reads each name in allowlist from os.Environ and returns
// the subset present.
func envFromHost(allowlist []string) map[string]string {
	out := map[string]string{}
	for _, name := range allowlist {
		if v, ok := os.LookupEnv(name); ok {
			out[name] = v
		}
	}
	return out
}

// skipIfNoClaude calls t.Skip with an actionable message when the
// `claude` binary is not on PATH. Used by the native wrapper only —
// the docker wrapper installs claude inside the lab container.
func skipIfNoClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude not on PATH (install via `npm install -g @anthropic-ai/claude-code` to run this test); err: %v", err)
	}
}

// skipIfNoAuth calls t.Skip with an actionable message when none of
// the known auth env vars are present in the host environment.
func skipIfNoAuth(t *testing.T) {
	t.Helper()
	env := envFromHost(claude.DefaultEnvAllowlist)
	if len(env) == 0 {
		t.Skipf("no Claude auth env var present (set one of %v) to run this test", claude.DefaultEnvAllowlist)
	}
}
