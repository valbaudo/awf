package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

// testLayer2Contract is Bucket 15 (Phase 5 slice 5.2). Validates that the
// engine's typed-output validation works for adapters reporting
// Caps{NativeSchema: false} — i.e. layer-2 adapters whose Output came from
// a structuring-call extraction rather than native --json-schema validation.
//
//   - non_native_schema_accepts_typed_output: fake with NativeSchema:false
//     returns schema-conforming Output → engine accepts → ok.
//   - non_native_schema_rejects_schema_violation: fake returns non-conforming
//     Output → engine rejects with *agent.ErrUnparseableOutput →
//     retryable_failure → run halts after retries exhausted.
//   - non_native_schema_propagates_launch_error: adapter returns
//     *agent.ErrAgentLaunch → retryable_failure → run halts.
func testLayer2Contract(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("non_native_schema_accepts_typed_output", func(t *testing.T) { testLayer2Accepts(t, factory) })
	t.Run("non_native_schema_rejects_schema_violation", func(t *testing.T) { testLayer2Rejects(t, factory) })
	t.Run("non_native_schema_propagates_launch_error", func(t *testing.T) { testLayer2PropagatesError(t, factory) })
}

func testLayer2Accepts(t *testing.T, factory BackendFactory) {
	t.Helper()
	register := func(reg *agent.Registry) {
		fk := fake.New("test/non-native-schema").
			WithCaps(agent.Caps{NativeSchema: false}).
			Script(0, fake.Result{Output: map[string]any{"topic": "frogs", "sentiment": "positive"}})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, layer2ContractWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
}

func testLayer2Rejects(t *testing.T, factory BackendFactory) {
	t.Helper()
	register := func(reg *agent.Registry) {
		// `sentiment` missing — schema-required field absent → ValidateOutputMap fails.
		fk := fake.New("test/non-native-schema").
			WithCaps(agent.Caps{NativeSchema: false}).
			Script(0, fake.Result{Output: map[string]any{"topic": "frogs"}}).
			Script(1, fake.Result{Output: map[string]any{"topic": "frogs"}}). // retry attempt also bad
			Script(2, fake.Result{Output: map[string]any{"topic": "frogs"}})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, layer2ContractWorkflow, register)
	oc, err := h.runWorkflow(t)
	if oc != engine.OutcomeRetryableFailure {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeRetryableFailure)
	}
	var unparseable *agent.ErrUnparseableOutput
	if !errors.As(err, &unparseable) {
		t.Errorf("err = %v, want *agent.ErrUnparseableOutput", err)
	}
}

func testLayer2PropagatesError(t *testing.T, factory BackendFactory) {
	t.Helper()
	transport := &agent.ErrAgentLaunch{Cause: errors.New("layer-2 extractor: model overloaded")}
	register := func(reg *agent.Registry) {
		adapter := &layer2ErrorAdapter{
			Fake:      fake.New("test/non-native-schema").WithCaps(agent.Caps{NativeSchema: false}),
			launchErr: transport,
		}
		if err := reg.Register(adapter); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, layer2ContractWorkflow, register)
	oc, err := h.runWorkflow(t)
	if oc != engine.OutcomeRetryableFailure {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeRetryableFailure)
	}
	if !errors.Is(err, transport.Cause) {
		t.Errorf("err does not wrap transport cause: %v", err)
	}
}

// layer2ErrorAdapter wraps fake.Fake to force a launch error — simulates
// what a real layer-2 adapter would surface if its extractor failed
// (Appendix H of the Phase 5 design doc).
type layer2ErrorAdapter struct {
	*fake.Fake
	launchErr error
}

func (a *layer2ErrorAdapter) Launch(_ context.Context, _ container.Handle, _ agent.AgentInvocation) (agent.AgentResult, <-chan agent.AgentEvent, error) {
	return agent.AgentResult{}, nil, a.launchErr
}
