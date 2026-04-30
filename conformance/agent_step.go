package conformance

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// testAgentStep is Bucket 12 (Phase 5 slice 5.2). Three sub-tests:
//
//   - typed_output_committed: agent/fake.Launch returns typed Output;
//     node.completed has Outputs that match the schema; downstream refs
//     would see them (no downstream step in this fixture — the round-trip
//     through Blobs is the assertion).
//   - validate_rejects_unknown_with: a rejectingAdapter registered under
//     the same Ref makes the dispatch fail with *agent.ErrInvalidConfig →
//     permanent_failure; run halts.
//   - unresolved_uses_halts_run: AgentStep references a `uses:` ref no
//     registered adapter satisfies → *agent.ErrAdapterNotFound surfaces
//     as an internal halt (engine.Run returns ("", wrappedError) — no
//     node.failed entry; see engine/agent_step.go:122-126 for the
//     dr.Outcome == "" split). (Engine-side peer of slice 5.1's
//     cli/resume drift check — see plan prose for the re-interpretation.)
func testAgentStep(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("typed_output_committed", func(t *testing.T) { testAgentTypedOutputCommitted(t, factory) })
	t.Run("validate_rejects_unknown_with", func(t *testing.T) { testAgentValidateRejects(t, factory) })
	t.Run("unresolved_uses_halts_run", func(t *testing.T) { testAgentUnresolvedUsesHalts(t, factory) })
}

func testAgentTypedOutputCommitted(t *testing.T, factory BackendFactory) {
	t.Helper()
	register := func(reg *agent.Registry) {
		fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
			Output: map[string]any{"verdict": "approved", "confidence": 0.95},
		})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, agentStepBasicWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
	// Read node.completed via RunState (built by Fold in the harness).
	events, ferr := h.log.Fold()
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}
	rs, ferr := engine.Fold(events, h.blobs)
	if ferr != nil {
		t.Fatalf("engine.Fold: %v", ferr)
	}
	nr, ok := rs.LookupCompleted("triage")
	if !ok {
		t.Fatalf("Completed[triage] missing")
	}
	if nr.Outputs["verdict"] != "approved" {
		t.Errorf("Outputs[verdict] = %v, want %q", nr.Outputs["verdict"], "approved")
	}
}

func testAgentValidateRejects(t *testing.T, factory BackendFactory) {
	t.Helper()
	register := func(reg *agent.Registry) {
		if err := reg.Register(&validateRejecter{
			Fake: fake.New("anthropic/claude-code"),
			err:  &agent.ErrInvalidConfig{Ref: "anthropic/claude-code", Key: "prompt", Reason: "forbidden"},
		}); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, agentStepBasicWorkflow, register)
	oc, err := h.runWorkflow(t)
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomePermanentFailure)
	}
	var badConfig *agent.ErrInvalidConfig
	if !errors.As(err, &badConfig) {
		t.Errorf("err = %v, want *agent.ErrInvalidConfig", err)
	}
}

func testAgentUnresolvedUsesHalts(t *testing.T, factory BackendFactory) {
	t.Helper()
	// Register an adapter under a DIFFERENT ref than the workflow names.
	// Resolver.Lookup("anthropic/claude-code") misses → dispatcher returns
	// *agent.ErrAdapterNotFound → runAgentStep maps to permanent_failure →
	// run halts. This is the engine-surface peer of slice 5.1's cli/resume
	// drift check (which is fully covered by TestErrRuntimeDrift_* in
	// cli/resume_test.go); slice 5.2 does NOT duplicate that coverage.
	register := func(reg *agent.Registry) {
		if err := reg.Register(fake.New("test/some-other-adapter")); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, agentStepBasicWorkflow, register)
	oc, err := h.runWorkflow(t)
	// ErrAdapterNotFound is an infrastructure miss (no adapter registered for the
	// ref). The dispatcher returns it as a plain error with Outcome=="", so
	// runAgentStep propagates it as ("", error) — an internal halt, NOT a step
	// node.failed permanent_failure. The run halts with a non-nil error; the
	// outcome is the empty string.
	if err == nil {
		t.Fatalf("expected non-nil error for unresolved adapter, got oc=%q", oc)
	}
	var notFound *agent.ErrAdapterNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want *agent.ErrAdapterNotFound", err)
	}
	if notFound.Ref != "anthropic/claude-code" {
		t.Errorf("Ref = %q, want %q", notFound.Ref, "anthropic/claude-code")
	}
}

// validateRejecter is a fake.Fake subclass that overrides ValidateConfig
// for Bucket 12 validate-rejects sub-test. (engine/local_dispatcher_test.go
// has a sibling type with the same purpose; can't share — _test.go files
// don't cross packages.)
type validateRejecter struct {
	*fake.Fake
	err error
}

func (r *validateRejecter) ValidateConfig(_ ir.RawConfig) error { return r.err }
