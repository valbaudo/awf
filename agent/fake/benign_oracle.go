package fake

import "github.com/valbaudo/awf/agent"

// NewBenignOracle returns a pre-scripted Fake adapter intended for Phase 5
// Bucket 13 gate tests (and Phase 5.4 benign-payload-oracle fixtures). The
// adapter is registered under ref "test/oracle" (NOT "anthropic/claude-code")
// so test fixtures distinguish it from the real claude ref.
//
// Script (position-driven — NOT feedback-driven):
//   - Call index 0: {verified: false, fooled_by_benign: true,
//     feedback: "exploit was fake — only matched a version string"}
//   - Call index 1: {verified: true, fooled_by_benign: false, feedback: ""}
//
// IMPORTANT: This fake is purely POSITIONAL. It does NOT inspect
// AgentInvocation.Feedback or AgentInvocation.With to decide which result
// to return — it just consumes scripts in call order. The narrative ("the
// generator produced a real exploit after the gate threaded feedback into
// attempt 2") only HOLDS if the slice 5.2 dispatcher correctly threads
// feedback. This fake doesn't verify that threading happened.
//
// To verify feedback threading in slice 5.2's Bucket 13b, inspect
// fake.Calls() AFTER the gate runs: Calls()[1].With (the attempt-2
// generator input) should contain the prior verdict's "feedback" field
// substituted in. That's the assertion slice 5.2's test makes — NOT this
// fake's responsibility.
//
// Gate `until` expression for the workflow: `evaluate.verified && !evaluate.fooled_by_benign`.
// Call 0 → until is false → gate repairs. Call 1 → until is true → gate passes.
//
// max_attempts: 2 minimum (slice 5.4 fixture sets this). Adding more
// Script entries here is harmless — Launch consumes the scripts in
// order and returns an out-of-bounds error after they're exhausted.
func NewBenignOracle() *Fake {
	return New("test/oracle").
		Script(0, Result{
			Output: map[string]any{
				"verified":         false,
				"fooled_by_benign": true,
				"feedback":         "exploit was fake — only matched a version string",
			},
		}).
		Script(1, Result{
			Output: map[string]any{
				"verified":         true,
				"fooled_by_benign": false,
				"feedback":         "",
			},
		})
}

// Ensure *Fake satisfies agent.Adapter (compile-time check).
var _ agent.Adapter = (*Fake)(nil)
