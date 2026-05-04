package conformance

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
)

// AgentBackendFactory mints one (Backend, Adapter, Spec) tuple per
// sub-test in conformance.RunAgentSuite. Sibling of Phase 4's
// DockerBackendFactory.
//
// Slice 5.4 ships two factory closures (in
// conformance_agent_claude_{native,docker}_test.go):
//   - nativeFactory: Spec.Name only; Spec.Compose == nil
//   - dockerFactory: Spec.Name + Spec.Compose + Spec.Service
//
// Bucket 14c checks Spec.Compose != nil to detect "this factory
// provides multi-service container isolation" and skips otherwise.
// No separate Caps struct — Spec.Compose IS the capability signal.
type AgentBackendFactory func(t *testing.T) AgentTestEnv

// AgentTestEnv carries everything a Bucket 14 sub-test needs.
type AgentTestEnv struct {
	Backend container.Backend
	Adapter agent.Adapter
	Spec    container.ContainerSpec
}

// RunAgentSuite is the single entry point. Sub-tests run independently;
// 14c self-skips when Spec.Compose == nil.
//
// Bucket 14b (schema-impossible prompt → *ErrUnparseableOutput) is
// NOT in this suite — it ships as a unit test in
// agent/claude/launch_test.go using a hand-crafted stream-json fixture
// (slice 5.4 r1 revision 5).
func RunAgentSuite(t *testing.T, factory AgentBackendFactory) {
	t.Helper()
	t.Run("bucket14a_simple_schema", func(t *testing.T) { testBucket14aSimpleSchema(t, factory) })
	t.Run("bucket14c_gate_under_real_claude", func(t *testing.T) { testBucket14cGateUnderRealClaude(t, factory) })
}
