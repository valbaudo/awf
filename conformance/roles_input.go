package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// rolesInputWorkflow — Task 7 case 1/7/8 fixture: a root-level role `r` whose
// model: is a raw {{ input.model }} template (AWF1067's input-only surface).
// One agent step `triage` resolves uses: r with no step-local with:.
var rolesInputWorkflow = fmt.Sprintf(`workflow: conformance-roles-input
version: 1
input_schema:
  type: object
  additionalProperties: false
  required: [model]
  properties:
    model: { type: string }
containers:
  lab:
    image: %s
agents:
  r:
    uses: anthropic/claude-code
    model: "{{ input.model }}"
graph:
  - id: triage
    container: lab
    uses: r
    output_schema:
      type: object
      additionalProperties: false
      required: [verdict]
      properties:
        verdict: { type: string }
`, fakeImageDigest)

// rolesInputOverlayWorkflow — Task 7 case 7: same role as rolesInputWorkflow,
// but triage carries a step-local with: model that must win over the
// role-resolved {{ input.model }} value (key-blind overlay, step wins).
var rolesInputOverlayWorkflow = fmt.Sprintf(`workflow: conformance-roles-input-overlay
version: 1
input_schema:
  type: object
  additionalProperties: false
  required: [model]
  properties:
    model: { type: string }
containers:
  lab:
    image: %s
agents:
  r:
    uses: anthropic/claude-code
    model: "{{ input.model }}"
graph:
  - id: triage
    container: lab
    uses: r
    with:
      model: explicit-override
    output_schema:
      type: object
      additionalProperties: false
      required: [verdict]
      properties:
        verdict: { type: string }
`, fakeImageDigest)

// rolesInputCallRootWorkflow / rolesInputCallChildWorkflow — Task 7 cases 2/3:
// the root forwards its own input.model into the child's call input; the
// child declares its OWN role `r` (input-parameterized by the CHILD's own
// input, per the man page's "owning module" scoping rule) and a single agent
// step resolving it. No container of its own — the root only calls.
var rolesInputCallRootWorkflow = `workflow: conformance-roles-input-call-root
version: 1
input_schema:
  type: object
  additionalProperties: false
  required: [model]
  properties:
    model: { type: string }
imports:
  child: child.awf.yaml
containers: {}
graph:
  - id: child_call
    call: child
    input:
      model: "{{ input.model }}"
`

var rolesInputCallChildWorkflow = fmt.Sprintf(`workflow: conformance-roles-input-call-child
version: 1
input_schema:
  type: object
  additionalProperties: false
  required: [model]
  properties:
    model: { type: string }
output_schema:
  type: object
  additionalProperties: false
  required: [verdict]
  properties:
    verdict: { type: string }
outputs:
  verdict: "{{ step.triage.verdict }}"
containers:
  lab:
    image: %s
agents:
  r:
    uses: anthropic/claude-code
    model: "{{ input.model }}"
graph:
  - id: triage
    container: lab
    uses: r
    output_schema:
      type: object
      additionalProperties: false
      required: [verdict]
      properties:
        verdict: { type: string }
`, fakeImageDigest)

// testRolesInput is the Task 7 conformance bucket: end-to-end behavior for
// input-parameterizable agent roles (spec §9), fake backend only. Cases 5
// (non-input scope) and 6 (nested template) are AWF1067 static-validation
// faults already covered end-to-end by ir/validate_agents_test.go
// (TestRoleTemplate_NonInputRoot_Rejected, TestRoleTemplate_NestedTemplate_Rejected,
// TestRoleTemplate_NestedMapKey_Rejected) from Task 2 — not duplicated here.
func testRolesInput(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("root_selection_two_runs_differ", func(t *testing.T) {
		testRolesInputRootSelection(t, factory)
	})
	t.Run("child_selection_forwarded_input", func(t *testing.T) {
		testRolesInputChildSelection(t, factory)
	})
	t.Run("resume_determinism_replays_recorded_call_input", func(t *testing.T) {
		testRolesInputResumeDeterminism(t, factory)
	})
	t.Run("digest_stability_across_input_values", func(t *testing.T) {
		testRolesInputDigestStability(t, factory)
	})
	t.Run("overlay_precedence_step_with_wins", func(t *testing.T) {
		testRolesInputOverlayPrecedence(t, factory)
	})
	t.Run("omitted_input_fails_honestly", func(t *testing.T) {
		testRolesInputOmittedHonest(t, factory)
	})
}

// registerRolesInputRole wires the fake base adapter + a DerivedAdapter for
// role "r" (root module: AgentRuntimeRef leaves the name bare) with a raw
// {{ input.model }} role with:, mirroring cli/agent_registry.go's
// registerRolesForModule for a single-module (non-call) fixture.
func registerRolesInputRole(t *testing.T, reg *agent.Registry) *fake.Fake {
	t.Helper()
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{
		Output: map[string]any{"verdict": "clean"},
	})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register base: %v", err)
	}
	if err := reg.Register(agent.NewDerivedAdapter("r", fk, ir.RawConfig{"model": "{{ input.model }}"})); err != nil {
		t.Fatalf("Register role: %v", err)
	}
	return fk
}

// testRolesInputRootSelection — case 1: two independent runs of the SAME
// root-role fixture with different --input model values. The recorded
// invocation the fake BASE adapter received must carry each run's own
// resolved model, proving the role's {{ input.model }} template resolves
// against the run's actual input at step execution time (not a fixed value
// baked into the role at load time).
func testRolesInputRootSelection(t *testing.T, factory BackendFactory) {
	t.Helper()

	run := func(model string) *fake.Fake {
		var fk *fake.Fake
		h := newHarnessWithAgentRegistry(t, factory, rolesInputWorkflow, func(reg *agent.Registry) {
			fk = registerRolesInputRole(t, reg)
		})
		h.input = map[string]any{"model": model}
		oc, err := h.runWorkflow(t)
		if err != nil {
			t.Fatalf("runWorkflow(model=%s): %v", model, err)
		}
		if oc != engine.OutcomeOK {
			t.Fatalf("Outcome(model=%s) = %q, want %q", model, oc, engine.OutcomeOK)
		}
		return fk
	}

	fkA := run("gpt-a")
	fkB := run("gpt-b")

	callsA, callsB := fkA.Calls(), fkB.Calls()
	if len(callsA) != 1 {
		t.Fatalf("run A: fake Launch count = %d, want 1", len(callsA))
	}
	if len(callsB) != 1 {
		t.Fatalf("run B: fake Launch count = %d, want 1", len(callsB))
	}
	if callsA[0].With["model"] != "gpt-a" {
		t.Errorf("run A: base adapter With[model] = %v, want %q", callsA[0].With["model"], "gpt-a")
	}
	if callsB[0].With["model"] != "gpt-b" {
		t.Errorf("run B: base adapter With[model] = %v, want %q", callsB[0].With["model"], "gpt-b")
	}
}

// testRolesInputChildSelection — case 2: the root forwards its own
// input.model into the child's call input; the child's role `r` resolves
// {{ input.model }} against the CHILD's own input (the owning module), which
// is exactly the value the root forwarded via call: input:. Proves scoping
// crosses a call boundary correctly (not the root's raw --input, and not
// some fixed default).
func testRolesInputChildSelection(t *testing.T, factory BackendFactory) {
	t.Helper()

	roleRef := engine.AgentRuntimeRef(&ir.Workflow{Agents: map[string]ir.AgentRole{"r": {Uses: "anthropic/claude-code"}}}, "child", "r")

	var fk *fake.Fake
	h := newHarnessWithAgentRegistry(t, factory, rolesInputCallRootWorkflow, func(reg *agent.Registry) {
		fk = fake.New("anthropic/claude-code").Script(0, fake.Result{
			Output: map[string]any{"verdict": "clean"},
		})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register base: %v", err)
		}
		if err := reg.Register(agent.NewDerivedAdapter(roleRef, fk, ir.RawConfig{"model": "{{ input.model }}"})); err != nil {
			t.Fatalf("Register role: %v", err)
		}
	})
	writeSubworkflowFile(t, h, "child.awf.yaml", rolesInputCallChildWorkflow)
	h.input = map[string]any{"model": "forwarded-model"}

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake Launch count = %d, want 1", len(calls))
	}
	if calls[0].With["model"] != "forwarded-model" {
		t.Errorf("child step base adapter With[model] = %v, want %q (the root's input.model forwarded via call: input:)", calls[0].With["model"], "forwarded-model")
	}
}

// testRolesInputResumeDeterminism — case 3: seeds a log where the call
// boundary is already committed (call.started) but the child's role-bound
// agent step has NOT run yet, then resumes. The seeded call.started carries
// a recorded input.model DIFFERENT from anything the root's own (absent)
// input could produce — root.started here carries NO input at all, so if
// the engine ever re-evaluated the call's input: {{ input.model }} template
// on resume (instead of replaying the recorded call.started), it would hard
// fail on an unresolved reference. It doesn't: the child's role resolves to
// exactly the recorded value, proving resume replays call.started rather
// than recomputing it (man page: "resolved value rides the pinned run/call
// input and is replayed on resume").
func testRolesInputResumeDeterminism(t *testing.T, factory BackendFactory) {
	t.Helper()

	roleRef := engine.AgentRuntimeRef(&ir.Workflow{Agents: map[string]ir.AgentRole{"r": {Uses: "anthropic/claude-code"}}}, "child", "r")

	var fk *fake.Fake
	h := newHarnessWithAgentRegistry(t, factory, rolesInputCallRootWorkflow, func(reg *agent.Registry) {
		fk = fake.New("anthropic/claude-code").Script(0, fake.Result{
			Output: map[string]any{"verdict": "clean"},
		})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register base: %v", err)
		}
		if err := reg.Register(agent.NewDerivedAdapter(roleRef, fk, ir.RawConfig{"model": "{{ input.model }}"})); err != nil {
			t.Fatalf("Register role: %v", err)
		}
	})
	writeSubworkflowFile(t, h, "child.awf.yaml", rolesInputCallChildWorkflow)

	ld := loadSubworkflowDefinition(t, h)
	digest, err := ld.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	appendEvent(t, h.log, state.Event{
		Type: engine.EventRunStarted,
		// Deliberately NO InputRef: root has no recorded input at all, so a
		// resume-time recompute of the call's {{ input.model }} template
		// would fail outright — only the recorded call.started can supply it.
		Data: mustJSON(t, engine.RunStartedData{RunID: h.runID, WorkflowDigest: digest}),
	})

	inputRaw, err := json.Marshal(map[string]any{"model": "recorded-model"})
	if err != nil {
		t.Fatalf("marshal call input: %v", err)
	}
	inputRef, err := h.blobs.Put(inputRaw)
	if err != nil {
		t.Fatalf("put call input: %v", err)
	}

	ver, verr := fk.Version(context.Background(), container.Handle{})
	if verr != nil {
		t.Fatalf("fk.Version: %v", verr)
	}
	runtimeParent := engine.CallWorkflowRuntimePath("child_call")
	seededRuntimes := []engine.ResolvedRuntime{{
		Ref:       roleRef,
		Version:   ver,
		Container: engine.QualifiedContainerKey(runtimeParent, "lab"),
	}}
	appendEvent(t, h.log, state.Event{
		Type: engine.EventCallStarted,
		Path: "child_call",
		Data: mustJSON(t, engine.CallStartedData{InputRef: inputRef, Runtimes: seededRuntimes}),
	})

	oc, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("resume Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake Launch count = %d, want 1", len(calls))
	}
	if calls[0].With["model"] != "recorded-model" {
		t.Errorf("resumed child step base adapter With[model] = %v, want %q (replayed from call.started, not recomputed)", calls[0].With["model"], "recorded-model")
	}
}

// testRolesInputDigestStability — case 4: the same workflow definition run
// with two different --input model values must produce the IDENTICAL
// WorkflowDigest — the raw {{ input.model }} template folds into the
// definition digest, never the resolved value.
func testRolesInputDigestStability(t *testing.T, factory BackendFactory) {
	t.Helper()

	digestFor := func(model string) string {
		h := newHarnessWithAgentRegistry(t, factory, rolesInputWorkflow, func(reg *agent.Registry) {
			registerRolesInputRole(t, reg)
		})
		h.input = map[string]any{"model": model}
		oc, err := h.runWorkflow(t)
		if err != nil {
			t.Fatalf("runWorkflow(model=%s): %v", model, err)
		}
		if oc != engine.OutcomeOK {
			t.Fatalf("Outcome(model=%s) = %q, want %q", model, oc, engine.OutcomeOK)
		}
		return mustRunStartedData(t, h).WorkflowDigest
	}

	dA := digestFor("gpt-a")
	dB := digestFor("gpt-b")
	if dA == "" {
		t.Fatal("WorkflowDigest is empty")
	}
	if dA != dB {
		t.Errorf("WorkflowDigest(model=gpt-a) = %q, WorkflowDigest(model=gpt-b) = %q; want equal (the raw template folds into the digest, not the resolved value)", dA, dB)
	}
}

// testRolesInputOverlayPrecedence — case 7: a step-local with: model must
// override the role-resolved {{ input.model }} value for that one step (the
// engine's key-blind overlay, step wins — same mechanism testRoles already
// proves for a literal role default; here the role side is a template).
func testRolesInputOverlayPrecedence(t *testing.T, factory BackendFactory) {
	t.Helper()

	var fk *fake.Fake
	h := newHarnessWithAgentRegistry(t, factory, rolesInputOverlayWorkflow, func(reg *agent.Registry) {
		fk = registerRolesInputRole(t, reg)
	})
	h.input = map[string]any{"model": "from-input"}

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake Launch count = %d, want 1", len(calls))
	}
	if calls[0].With["model"] != "explicit-override" {
		t.Errorf("base adapter With[model] = %v, want %q (step with: must win over the role-resolved input.model)", calls[0].With["model"], "explicit-override")
	}
}

// testRolesInputOmittedHonest — case 8: a role referencing {{ input.model }}
// run with NO --input must fail the step honestly (permanent_failure, zero
// adapter launches) — never materialize an input_schema default: or resolve
// to a silent empty string (man page: "an omitted --input does not
// materialize input-schema default:s").
func testRolesInputOmittedHonest(t *testing.T, factory BackendFactory) {
	t.Helper()

	var fk *fake.Fake
	h := newHarnessWithAgentRegistry(t, factory, rolesInputWorkflow, func(reg *agent.Registry) {
		fk = registerRolesInputRole(t, reg)
	})
	// h.input intentionally left nil: no --input supplied.

	oc, err := h.runWorkflow(t)
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q (err=%v), want %q — an omitted --input must fail the step, not silently resolve", oc, err, engine.OutcomePermanentFailure)
	}
	if err == nil {
		t.Error("runWorkflow err = nil, want a substitution/unresolved-reference error")
	}
	if len(fk.Calls()) != 0 {
		t.Errorf("fake Launch count = %d, want 0 (no default silently materialized into a successful launch)", len(fk.Calls()))
	}
}
