//go:build integ && live

package conformance

import (
	"os"
	"os/exec"
	"testing"

	"github.com/valbaudo/awf/agent/goose"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

func skipIfNoGoose(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("goose"); err != nil {
		t.Skipf("goose not on PATH; err: %v", err)
	}
}

// skipIfNoGooseLive gates on an explicit opt-in: the claude-code provider has NO
// static auth probe (claude can be installed but not logged in), so the operator
// asserts a working goose+provider stack via AWF_GOOSE_LIVE. For anthropic/openai,
// the provider key in the host env is the natural gate; AWF_GOOSE_LIVE covers all.
func skipIfNoGooseLive(t *testing.T) {
	t.Helper()
	if os.Getenv("AWF_GOOSE_LIVE") == "" {
		t.Skip("AWF_GOOSE_LIVE not set; skipping real-binary goose conformance (no static auth probe for claude-code)")
	}
}

// TestConformanceAgentGooseNative drives RunAgentSuite against the native backend +
// host `goose`. Bucket 14a (typed-output round-trip via the layer-2 path) runs; 14c
// skips (Spec.Compose == nil). Best-effort smoke — NOT the definition-of-done (that
// is the fake-backed Bucket 15, which runs in `make test`).
func TestConformanceAgentGooseNative(t *testing.T) {
	skipIfNoGoose(t)
	skipIfNoGooseLive(t)

	nativeFactory := func(t *testing.T) AgentTestEnv {
		t.Helper()
		nb, err := native.New(t.TempDir())
		if err != nil {
			t.Fatalf("native.New: %v", err)
		}
		ad, err := goose.New(
			goose.WithEnv(envFromHost(goose.DefaultEnvAllowlist)),
			goose.WithBackend(nb),
		)
		if err != nil {
			t.Fatalf("goose.New: %v", err)
		}
		return AgentTestEnv{
			Backend: nb,
			Adapter: ad,
			Spec:    container.ContainerSpec{Name: "lab"},
		}
	}

	RunAgentSuite(t, nativeFactory)
}
