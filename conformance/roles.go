package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// rolesWorkflow — C3 (Task 6). A top-level agents: role `auditor` binds the
// fake base adapter (uses: anthropic/claude-code) plus opaque defaults: a model,
// a system_prompt, and a with: carrying the fleet memory MCP handle
// (mcp_servers). One container `lab`. One agent step `triage` resolves
// uses: auditor and overrides model via its own step-local with:; the engine's
// key-blind overlay places the step with: ON TOP of the role with:.
var rolesWorkflow = fmt.Sprintf(`workflow: conformance-roles
version: 1
containers:
  lab:
    image: %s
agents:
  auditor:
    uses: anthropic/claude-code
    model: opus
    system_prompt: "audit"
    with:
      mcp_servers: [memclaw]
graph:
  - id: triage
    container: lab
    uses: auditor
    with:
      model: sonnet
    output_schema:
      type: object
      additionalProperties: false
      required: [verdict]
      properties:
        verdict: { type: string }
`, fakeImageDigest)

// testRoles is the C3 conformance bucket (Task 6): a role-wired agent step plus
// the memory-MCP-handle pass-through. It proves three things end-to-end on the
// fake backend:
//
//  1. Role resolves + step runs — uses: auditor resolves to the DerivedAdapter
//     registered under the role name; triage commits ok with its typed verdict
//     round-tripped through node.completed (Bucket 12's verdict assertion).
//  2. Merge correctness (the memory-handle proof) — the fake BASE adapter saw
//     inv.With == {model:"sonnet" (step override won), system_prompt:"audit"
//     (role), mcp_servers:["memclaw"] (role — the fleet memory handle)}. This is
//     the literal "AWF passes the memory MCP handle to the fleet" assertion.
//  3. Run-start pinning — run.started.Runtimes includes (ref="auditor",
//     container="lab"): the role is a first-class pinned runtime, drift-checked
//     on resume exactly like a base adapter ref.
//
// The role is registered the conformance equivalent of cli/registerRoles:
// the fake base goes in first, then a DerivedAdapter wrapping it under the role
// name with the role's model/system_prompt folded into its with: as opaque keys
// (mirroring cli/agent_registry.go roleWithFor). The DerivedAdapter's key-blind
// overlay (agent/derived.go) does the merge the engine relies on.
func testRoles(t *testing.T, factory BackendFactory) {
	t.Helper()

	// fk is captured so the bucket can read back the inv.With the BASE adapter
	// received after the DerivedAdapter's overlay (the memory-handle proof).
	var fk *fake.Fake
	register := func(reg *agent.Registry) {
		fk = fake.New("anthropic/claude-code").Script(0, fake.Result{
			Output: map[string]any{"verdict": "clean"},
		})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register base: %v", err)
		}
		// Conformance equivalent of cli/registerRoles: wrap the fake base under
		// the role name with the role's model/system_prompt folded into the
		// role with: as opaque keys (mirrors cli/agent_registry.go roleWithFor).
		roleWith := ir.RawConfig{
			"mcp_servers":   []any{"memclaw"},
			"model":         "opus",  // role default; the step overrides it below
			"system_prompt": "audit", // role default; no step override
		}
		if err := reg.Register(agent.NewDerivedAdapter("auditor", fk, roleWith)); err != nil {
			t.Fatalf("Register role: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, rolesWorkflow, register)

	// Conformance equivalent of cli/runtimes.go resolveRuntimes: resolve the
	// step's (uses, container) pair through the registry so run.started records
	// the role as a first-class pinned runtime. uses: auditor → the
	// DerivedAdapter, whose Version delegates to the fake base ("fake-v1").
	roleAdapter, ok := h.agentRegistry.Lookup("auditor")
	if !ok {
		t.Fatalf("registry Lookup(auditor) miss after registerRoles equivalent")
	}
	ver, verr := roleAdapter.Version(context.Background(), container.Handle{})
	if verr != nil {
		t.Fatalf("Version(auditor): %v", verr)
	}
	h.runtimes = []engine.ResolvedRuntime{{Ref: "auditor", Version: ver, Container: "lab"}}

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	events, ferr := h.log.Fold()
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}

	// (1) Role resolves + step runs: the typed verdict round-trips through
	// node.completed.
	rs, ferr := engine.Fold(events, h.blobs)
	if ferr != nil {
		t.Fatalf("engine.Fold: %v", ferr)
	}
	nr, ok := rs.LookupCompleted("triage")
	if !ok {
		t.Fatalf("Completed[triage] missing")
	}
	if nr.Outputs["verdict"] != "clean" {
		t.Errorf("Outputs[verdict] = %v, want %q", nr.Outputs["verdict"], "clean")
	}

	// (2) Merge correctness — the memory-handle proof. The fake BASE adapter
	// must have seen the role with: overlaid by the step with: (step wins).
	calls := fk.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake Launch count = %d, want 1", len(calls))
	}
	gotWith := map[string]any(calls[0].With)
	wantWith := map[string]any{
		"model":         "sonnet",         // step override won
		"system_prompt": "audit",          // role default
		"mcp_servers":   []any{"memclaw"}, // role — the fleet memory handle
	}
	if !reflect.DeepEqual(gotWith, wantWith) {
		t.Errorf("base adapter inv.With = %#v, want %#v", gotWith, wantWith)
	}

	// (3) Run-start pinning — the role is a first-class pinned runtime. Read
	// Runtimes back from the folded run.started event.
	if len(events) == 0 || events[0].Type != engine.EventRunStarted {
		t.Fatalf("first event = %q, want %q", eventType(events), engine.EventRunStarted)
	}
	var started engine.RunStartedData
	if uerr := json.Unmarshal(events[0].Data, &started); uerr != nil {
		t.Fatalf("unmarshal run.started: %v", uerr)
	}
	want := engine.ResolvedRuntime{Ref: "auditor", Version: ver, Container: "lab"}
	found := false
	for _, rt := range started.Runtimes {
		if rt == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("run.started.Runtimes = %#v, want to include %#v (role is a first-class pinned runtime)", started.Runtimes, want)
	}
}

// eventType is a tiny helper for the run.started assertion's error message:
// returns the first event's Type, or "<none>" for an empty log.
func eventType(events []state.Event) string {
	if len(events) == 0 {
		return "<none>"
	}
	return events[0].Type
}
