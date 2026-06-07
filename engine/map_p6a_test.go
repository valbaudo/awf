package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
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

// The engine must flag a map's runtime-resolved per-element image with
// PullIfAbsent so a real backend (docker) pulls it + requires a digest pin +
// captures the booted digest. The fake ignores the flag, so we assert the
// engine SET it on the spec it handed to Backend.Create (the wiring seam to the
// docker integ test, which proves docker honors it). Regression guard: dropping
// the flag would silently make every map.image run fail at dispatch on docker
// ("no such image" — docker.Create does not pull on the normal path).
func TestMapImageDispatchSetsPullIfAbsent(t *testing.T) {
	digestRef := "registry.example.com/app@sha256:" + strings.Repeat("a", 64)
	rig := newMapRig(t, ok("probe"))

	mapNode := &ir.Map{
		Over: "{{ input.items }}", As: "v", Container: testMapContainer,
		Image: "{{ v.image }}", Concurrency: 1,
		Body: ir.NodeList{&ir.CodeStep{ID: "p", Container: testMapContainer, Run: "probe"}},
	}
	wf := &ir.Workflow{
		ID: "p6a", Version: 1,
		// No declared image: the container is a runtime-resolved map.image target.
		Containers: map[string]ir.Container{testMapContainer: {}},
		Input: &ir.JSONSchema{
			"type":       "object",
			"properties": map[string]any{"items": map[string]any{"type": "array"}},
		},
		Graph: ir.NodeList{mapNode},
	}
	rs := NewRunState(testRunID, testDigest, runOverItems(map[string]any{"image": digestRef}))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("runMap: got (%q, %v), want (ok, nil)", oc, err)
	}

	var found *container.ContainerSpec
	for i := range rig.fake.CreateSpecs {
		if rig.fake.CreateSpecs[i].Image == digestRef {
			found = &rig.fake.CreateSpecs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no Backend.Create with the rendered image %q; specs=%+v", digestRef, rig.fake.CreateSpecs)
	}
	if !found.PullIfAbsent {
		t.Errorf("map.image Create spec PullIfAbsent = false, want true")
	}
}

// Per-item containers MUST get DISTINCT backend names: the docker backend derives
// the container name from spec.Name, so every item sharing n.Container collides on
// a concurrent Create (the bug the first real docker map run hit — the fake ignores
// Name, so the path went untested). Regression guard: each item's Create spec gets
// a unique "<container>-item-N" name.
func TestMapPerItemContainerNamesAreUnique(t *testing.T) {
	rig := newMapRig(t, ok("echo a"), ok("echo b"))
	wf := staticOverWorkflow("v", echoStep("v", nil), 2, nil)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, runOverItems("a", "b"))

	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("runMap: (%q, %v), want (ok, nil)", oc, err)
	}
	got := map[string]int{}
	for i := range rig.fake.CreateSpecs {
		got[rig.fake.CreateSpecs[i].Name]++
	}
	for _, want := range []string{testMapContainer + "-item-0", testMapContainer + "-item-1"} {
		if got[want] != 1 {
			t.Errorf("per-item container name %q count=%d, want 1 (names must be unique per item); CreateSpecs names=%v", want, got[want], got)
		}
	}
}
