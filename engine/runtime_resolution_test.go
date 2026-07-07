package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
)

func TestCheckRuntimesDriftVersionChangedIsTyped(t *testing.T) {
	recorded := []ResolvedRuntime{{Ref: "x", Version: "v1", Container: "lab"}}
	current := []ResolvedRuntime{{Ref: "x", Version: "v2", Container: "lab"}}

	err := CheckRuntimesDrift(recorded, current)
	var drift *ErrRuntimeDrift
	if !errors.As(err, &drift) {
		t.Fatalf("err = %v, want *ErrRuntimeDrift", err)
	}
	if drift.Ref != "x" || drift.Container != "lab" || drift.Recorded != "v1" || drift.Current != "v2" {
		t.Fatalf("drift = %+v, want x/lab v1->v2", drift)
	}
}

func TestAgentRuntimeRefQualifiesImportedModuleRoles(t *testing.T) {
	wf := &ir.Workflow{
		ID: "mod-scan",
		Agents: map[string]ir.AgentRole{
			"auditor": {Uses: "awf/llm"},
		},
	}

	got := AgentRuntimeRef(wf, "mod-scan", "auditor")
	if got == "auditor" {
		t.Fatalf("AgentRuntimeRef imported role = %q, want qualified internal ref", got)
	}
	if got == "awf/llm" {
		t.Fatalf("AgentRuntimeRef imported role = base adapter ref %q, want role ref", got)
	}
	if got == AgentRuntimeRef(wf, "", "auditor") {
		t.Fatalf("imported role ref = root role ref %q, want distinct refs", got)
	}
	if strings.Contains(got, "/") {
		t.Fatalf("imported role ref = %q, want internal ref without '/'", got)
	}
}

func TestAgentRuntimeRefLeavesRootRoleAndBaseAdapterRefsUnchanged(t *testing.T) {
	wf := &ir.Workflow{
		Agents: map[string]ir.AgentRole{
			"auditor": {Uses: "awf/llm"},
		},
	}

	if got := AgentRuntimeRef(wf, "", "auditor"); got != "auditor" {
		t.Fatalf("root role ref = %q, want backward-compatible raw role", got)
	}
	if got := AgentRuntimeRef(wf, "mod-scan", "awf/llm"); got != "awf/llm" {
		t.Fatalf("base adapter ref = %q, want unchanged", got)
	}
}

func TestWalkRuntimeRefsUsesModuleQualifiedRoleRefs(t *testing.T) {
	wf := &ir.Workflow{
		ID: "mod-scan",
		Agents: map[string]ir.AgentRole{
			"auditor": {Uses: "awf/llm"},
		},
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "audit", Uses: "auditor"},
		},
	}

	refs := WalkRuntimeRefs(wf.ID, "parent.workflow", wf)
	if len(refs) != 1 {
		t.Fatalf("WalkRuntimeRefs len = %d, want 1: %+v", len(refs), refs)
	}
	want := AgentRuntimeRef(wf, wf.ID, "auditor")
	if refs[0].Uses != want {
		t.Fatalf("WalkRuntimeRefs Uses = %q, want %q", refs[0].Uses, want)
	}
}

func TestWalkRuntimeRefsIncludesContainerlessMapBodyAgent(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Map{
				Over:        "input.items",
				As:          "item",
				Container:   "lab",
				Concurrency: intPtr(1),
				Body: ir.NodeList{
					&ir.AgentStep{ID: "audit", Uses: "awf/llm"},
				},
			},
		},
	}

	refs := WalkRuntimeRefs("", "", wf)
	if len(refs) != 1 {
		t.Fatalf("WalkRuntimeRefs len = %d, want 1: %+v", len(refs), refs)
	}
	if refs[0].Uses != "awf/llm" || refs[0].Container != "" {
		t.Fatalf("WalkRuntimeRefs[0] = %+v, want awf/llm containerless", refs[0])
	}
}

func TestWalkRuntimeRefsIncludesContainerlessComposeBodyAgent(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Compose{
				As:      "lab",
				From:    "step.make.files.compose",
				Service: "api",
				Body: ir.NodeList{
					&ir.AgentStep{ID: "audit", Uses: "awf/llm"},
				},
			},
		},
	}

	refs := WalkRuntimeRefs("", "", wf)
	if len(refs) != 1 {
		t.Fatalf("WalkRuntimeRefs len = %d, want 1: %+v", len(refs), refs)
	}
	if refs[0].Uses != "awf/llm" || refs[0].Container != "" {
		t.Fatalf("WalkRuntimeRefs[0] = %+v, want awf/llm containerless", refs[0])
	}
}

func TestWalkRuntimeRefsSkipsRuntimeCreatedContainerAliases(t *testing.T) {
	wf := &ir.Workflow{
		Graph: ir.NodeList{
			&ir.Map{
				Over:        "input.items",
				As:          "item",
				Container:   "dyn",
				Image:       "oci://repo/{{ item.image }}",
				Concurrency: intPtr(1),
				Body: ir.NodeList{
					&ir.AgentStep{ID: "dynamic_map", Uses: "test/map", Container: "dyn"},
					&ir.AgentStep{ID: "containerless_map", Uses: "awf/llm"},
				},
			},
			&ir.Compose{
				As:      "rt",
				From:    "step.make.files.compose",
				Service: "api",
				Body: ir.NodeList{
					&ir.AgentStep{ID: "dynamic_compose", Uses: "test/compose", Container: "rt:api"},
					&ir.AgentStep{ID: "containerless_compose", Uses: "awf/llm"},
				},
			},
		},
	}

	refs := WalkRuntimeRefs("", "", wf)
	if len(refs) != 1 {
		t.Fatalf("WalkRuntimeRefs len = %d, want one deduped containerless ref: %+v", len(refs), refs)
	}
	if refs[0].Uses != "awf/llm" || refs[0].Container != "" {
		t.Fatalf("WalkRuntimeRefs[0] = %+v, want awf/llm containerless", refs[0])
	}
}
