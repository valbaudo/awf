package engine

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// The per-item scope at itemPath resolves {{ <as>.image }} to the worklist
// element's image string (P6a). This is the load-bearing assumption dispatchItem
// relies on; prove it in isolation.
func TestMapImageRendersAtItemScope(t *testing.T) {
	wf := &ir.Workflow{
		ID: "p6a", Version: 1,
		Containers: map[string]ir.Container{"version_lab": {Resources: &ir.Resources{CPU: "1"}}},
		Graph: ir.NodeList{
			&ir.Map{
				Over: "{{ input.items }}", As: "v", Container: "version_lab",
				Image: "{{ v.image }}", Concurrency: 1,
				Body: ir.NodeList{&ir.CodeStep{ID: "probe", Container: "version_lab", Run: "true"}},
			},
		},
	}
	ref := "registry.example.com/app@sha256:" + strings.Repeat("a", 64)
	rs := NewRunState("r", "d", nil)
	rs.RecordMapItem("map[0]", MapItemRecord{N: 0, ItemValue: map[string]any{"image": ref}})

	scope := NewScope(rs, wf, ItemPath("map[0]", 0))
	got, err := template.Substitute("{{ v.image }}", scope)
	if err != nil {
		t.Fatalf("Substitute: %v (the item-path scope did not bind <as> — wrong anchor)", err)
	}
	if got != ref {
		t.Errorf("rendered = %q, want %q", got, ref)
	}
}
