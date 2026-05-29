//go:build integ && live

package conformance

import (
	"os"
	"os/exec"
	"testing"

	"github.com/valbaudo/awf/agent/droid"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
)

func skipIfNoDroid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("droid"); err != nil {
		t.Skipf("droid not on PATH; err: %v", err)
	}
}

func skipIfNoDroidAuth(t *testing.T) {
	t.Helper()
	if os.Getenv("FACTORY_API_KEY") == "" {
		t.Skip("FACTORY_API_KEY not set; skipping real-binary droid conformance")
	}
}

// TestConformanceAgentDroidNative drives RunAgentSuite against the native
// backend + host `droid` binary. Bucket 14a (typed-output round-trip via the
// layer-2 path) runs; 14c skips (Spec.Compose == nil).
func TestConformanceAgentDroidNative(t *testing.T) {
	skipIfNoDroid(t)
	skipIfNoDroidAuth(t)

	nativeFactory := func(t *testing.T) AgentTestEnv {
		t.Helper()
		nb, err := native.New(t.TempDir())
		if err != nil {
			t.Fatalf("native.New: %v", err)
		}
		ad, err := droid.New(
			droid.WithEnv(envFromHost(droid.DefaultEnvAllowlist)),
			droid.WithBackend(nb),
		)
		if err != nil {
			t.Fatalf("droid.New: %v", err)
		}
		return AgentTestEnv{
			Backend: nb,
			Adapter: ad,
			Spec:    container.ContainerSpec{Name: "lab"},
		}
	}

	RunAgentSuite(t, nativeFactory)
}
