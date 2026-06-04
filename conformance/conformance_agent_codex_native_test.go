//go:build integ && live

package conformance

import (
	"os"
	"os/exec"
	"testing"

	"github.com/valbaudo/awf/agent/codex"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

func skipIfNoCodex(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("codex not on PATH; err: %v", err)
	}
}

// skipIfNoCodexLive gates on an explicit opt-in: codex's ChatGPT-OAuth can be
// present-but-unauthorized with no reliable static probe, so the operator asserts
// a working codex stack via AWF_CODEX_LIVE. (OPENAI_API_KEY users still benefit
// from the explicit gate — it documents intent and keeps the tier free.)
func skipIfNoCodexLive(t *testing.T) {
	t.Helper()
	if os.Getenv("AWF_CODEX_LIVE") == "" {
		t.Skip("AWF_CODEX_LIVE not set; skipping real-binary codex conformance")
	}
}

// TestConformanceAgentCodexNative drives RunAgentSuite against the native backend +
// host `codex`. Bucket 14a (typed-output round-trip via the native --output-schema
// path) runs; 14c skips (Spec.Compose == nil). Best-effort smoke — NOT the
// definition-of-done (that is the fake-backed Bucket 14 in `make test`).
func TestConformanceAgentCodexNative(t *testing.T) {
	skipIfNoCodex(t)
	skipIfNoCodexLive(t)

	nativeFactory := func(t *testing.T) AgentTestEnv {
		t.Helper()
		nb, err := native.New(t.TempDir())
		if err != nil {
			t.Fatalf("native.New: %v", err)
		}
		ad, err := codex.New(
			codex.WithEnv(envFromHost(codex.DefaultEnvAllowlist)),
			codex.WithBackend(nb),
		)
		if err != nil {
			t.Fatalf("codex.New: %v", err)
		}
		return AgentTestEnv{
			Backend: nb,
			Adapter: ad,
			Spec:    container.ContainerSpec{Name: "lab"},
		}
	}

	RunAgentSuite(t, nativeFactory)
}
