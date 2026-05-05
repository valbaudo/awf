//go:build integ && live

package claude_test

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"

	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

// skipIfNoClaude skips the test cleanly when the claude binary isn't on
// PATH — `make integ` must work on a contributor's laptop without claude
// installed.
func skipIfNoClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH; install Claude Code or unset --tags=integ for this test")
	}
}

func TestClaudeAdapterVersionDetection(t *testing.T) {
	skipIfNoClaude(t)

	be, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	h, err := be.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = be.Destroy(context.Background(), h) })

	a, err := claude.New(claude.WithBackend(be))
	if err != nil {
		t.Fatalf("claude.New: %v", err)
	}
	ver, err := a.Version(context.Background(), h)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if ver == "" {
		t.Fatal("Version returned empty string")
	}
	// Loose semver shape check — leading X.Y.Z. (Decision 4: tolerant
	// to trailing build hash.)
	semverShape := regexp.MustCompile(`^\d+\.\d+\.\d+`)
	if !semverShape.MatchString(ver) {
		t.Logf("Version = %q (does not match leading semver; first-line literal fallback)", ver)
	} else {
		t.Logf("Version = %q", ver)
	}
}

// skipIfNoAuthEnv skips when neither ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN,
// nor CLAUDE_CODE_OAUTH_TOKEN is set on the host.
func skipIfNoAuthEnv(t *testing.T) {
	t.Helper()
	for _, name := range claude.DefaultEnvAllowlist {
		if os.Getenv(name) != "" {
			return
		}
	}
	t.Skip("no auth env (ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN / CLAUDE_CODE_OAUTH_TOKEN); skipping integ test")
}
