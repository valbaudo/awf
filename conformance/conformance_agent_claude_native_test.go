//go:build integ && live

package conformance

import (
	"testing"

	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

// TestConformanceAgentClaudeNative drives RunAgentSuite against the
// native backend + host `claude` binary. Bucket 14a runs; 14c skips
// (Spec.Compose == nil).
func TestConformanceAgentClaudeNative(t *testing.T) {
	skipIfNoClaude(t)
	skipIfNoAuth(t)

	nativeFactory := func(t *testing.T) AgentTestEnv {
		t.Helper()
		nb, err := native.New(t.TempDir())
		if err != nil {
			t.Fatalf("native.New: %v", err)
		}
		ad, err := claude.New(
			claude.WithEnv(envFromHost(claude.DefaultEnvAllowlist)),
			claude.WithBackend(nb),
		)
		if err != nil {
			t.Fatalf("claude.New: %v", err)
		}
		return AgentTestEnv{
			Backend: nb,
			Adapter: ad,
			Spec:    container.ContainerSpec{Name: "lab"},
		}
	}

	RunAgentSuite(t, nativeFactory)
}
