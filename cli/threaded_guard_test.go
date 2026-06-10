package cli

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

// regWith registers fk into a fresh registry; fails the test on a Register error.
func regWith(t *testing.T, fk *fake.Fake) *agent.Registry {
	t.Helper()
	var reg agent.Registry
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return &reg
}

// TestCheckThreaded_ContinuesAgainstNonThreaded_Errors is the T8 fail-fast:
// a step declaring continues: whose resolved adapter is NOT Threaded must be
// rejected at run start with *ErrThreadedRequired carrying both ids.
func TestCheckThreaded_ContinuesAgainstNonThreaded_Errors(t *testing.T) {
	fk := fake.New("anthropic/claude-code") // default Caps: Threaded false
	reg := regWith(t, fk)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "draft", Uses: "anthropic/claude-code", Container: "lab"},
		&ir.AgentStep{ID: "refine", Uses: "anthropic/claude-code", Container: "lab", Continues: "draft"},
	}}
	err := checkThreadedAdapters(wf, reg)
	var want *ErrThreadedRequired
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrThreadedRequired", err)
	}
	if want.StepID != "refine" || want.Ref != "anthropic/claude-code" {
		t.Fatalf("got %+v, want {StepID:refine, Ref:anthropic/claude-code}", want)
	}
}

// TestCheckThreaded_ContinuesAgainstThreaded_OK: the same continues: against a
// Threaded adapter passes (no error).
func TestCheckThreaded_ContinuesAgainstThreaded_OK(t *testing.T) {
	fk := fake.New("awf/llm").WithCaps(agent.Caps{Containerless: true, Threaded: true})
	reg := regWith(t, fk)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "draft", Uses: "awf/llm"},
		&ir.AgentStep{ID: "refine", Uses: "awf/llm", Continues: "draft"},
	}}
	if err := checkThreadedAdapters(wf, reg); err != nil {
		t.Fatalf("checkThreadedAdapters: %v, want nil", err)
	}
}

func TestCheckThreaded_LoadedDefinitionUsesChildQualifiedRoleRef(t *testing.T) {
	child := &ir.Workflow{
		Agents: map[string]ir.AgentRole{
			"auditor": {Uses: "awf/llm"},
		},
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "draft", Uses: "auditor"},
			&ir.AgentStep{ID: "refine", Uses: "auditor", Continues: "draft"},
		},
	}
	fk := fake.New(engine.AgentRuntimeRef(child, "mod-scan", "auditor")).
		WithCaps(agent.Caps{Containerless: true, Threaded: true})
	reg := regWith(t, fk)
	ld := &ir.LoadedDefinition{
		Workflow: &ir.Workflow{},
		Modules: map[string]*ir.LoadedModule{
			"":         {ID: "", Workflow: &ir.Workflow{}},
			"mod-scan": {ID: "mod-scan", Workflow: child},
		},
	}

	if err := checkThreadedAdaptersForLoadedDefinition(ld, reg); err != nil {
		t.Fatalf("checkThreadedAdaptersForLoadedDefinition: %v, want nil", err)
	}
}

// TestCheckThreaded_NoContinues_NotThreaded_OK: a non-Threaded adapter with NO
// continues: anywhere is fine — the guard only fires on a continues: step.
func TestCheckThreaded_NoContinues_NotThreaded_OK(t *testing.T) {
	fk := fake.New("anthropic/claude-code") // Threaded false
	reg := regWith(t, fk)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "draft", Uses: "anthropic/claude-code", Container: "lab"},
	}}
	if err := checkThreadedAdapters(wf, reg); err != nil {
		t.Fatalf("checkThreadedAdapters: %v, want nil", err)
	}
}

// TestCheckThreaded_ContinuesInsideMapBody_Errors: the walk descends into
// map.body (unlike walkAgentRefsNodes, which skips it) — a continues: inside a
// map body against a non-Threaded adapter is still rejected.
func TestCheckThreaded_ContinuesInsideMapBody_Errors(t *testing.T) {
	fk := fake.New("anthropic/claude-code") // Threaded false
	reg := regWith(t, fk)
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "ancestor", Uses: "anthropic/claude-code", Container: "lab"},
		&ir.Map{Body: ir.NodeList{
			&ir.AgentStep{ID: "branch", Uses: "anthropic/claude-code", Container: "lab", Continues: "ancestor"},
		}},
	}}
	err := checkThreadedAdapters(wf, reg)
	var want *ErrThreadedRequired
	if !errors.As(err, &want) || want.StepID != "branch" {
		t.Fatalf("err = %v, want *ErrThreadedRequired{StepID:branch}", err)
	}
}

// TestCheckThreaded_AdapterNotFound_PropagatesLookupMiss: a continues: step
// whose uses: resolves to no adapter returns *agent.ErrAdapterNotFound (NOT a
// silent pass) — resolveRuntimes would also catch this, but the guard runs the
// same Lookup and must not treat a miss as "not threaded".
func TestCheckThreaded_AdapterNotFound_PropagatesLookupMiss(t *testing.T) {
	var reg agent.Registry // empty
	wf := &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{ID: "draft", Uses: "missing/agent"},
		&ir.AgentStep{ID: "refine", Uses: "missing/agent", Continues: "draft"},
	}}
	err := checkThreadedAdapters(wf, &reg)
	var want *agent.ErrAdapterNotFound
	if !errors.As(err, &want) || want.Ref != "missing/agent" {
		t.Fatalf("err = %v, want *agent.ErrAdapterNotFound{Ref:missing/agent}", err)
	}
}
