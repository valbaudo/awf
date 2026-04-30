package conformance

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
)

func TestNewHarnessWithAgentRegistry_StoresResolver(t *testing.T) {
	factory := func() container.Backend { return container.NewFake() }
	wfYAML := `workflow: t
version: 1
containers: {}
graph: []
`
	register := func(reg *agent.Registry) {
		if err := reg.Register(fake.New("test/x")); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, wfYAML, register)
	if h.agentRegistry == nil {
		t.Fatalf("agentRegistry = nil; want populated")
	}
	if _, ok := h.agentRegistry.Lookup("test/x"); !ok {
		t.Errorf("Lookup(\"test/x\") = false; want true")
	}
}

// TestNewHarness_AgentRegistry_NonNilByDefault pins the invariant introduced
// in Task 12 to defuse the typed-nil interface gotcha: even harnesses NOT
// constructed via newHarnessWithAgentRegistry must carry a non-nil empty
// Registry, so dispatcher.Resolver is never a typed-nil interface.
func TestNewHarness_AgentRegistry_NonNilByDefault(t *testing.T) {
	factory := func() container.Backend { return container.NewFake() }
	h := newHarness(t, factory, "workflow: t\nversion: 1\ncontainers: {}\ngraph: []\n")
	if h.agentRegistry == nil {
		t.Fatalf("base newHarness left agentRegistry = nil; want non-nil empty Registry (typed-nil interface bug guard)")
	}
	if got := h.agentRegistry.Refs(); len(got) != 0 {
		t.Errorf("agentRegistry.Refs() = %v, want empty", got)
	}
}
