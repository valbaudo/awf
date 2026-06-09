package engine_test

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/skillroute"
	"github.com/valbaudo/awf/state"
)

func TestRunAgentStep_SkillsFreshAppendsSelectionBeforeDispatch(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), nil)
	rs := engine.NewRunState("r1", "d", nil)
	rs.Assets = assets
	log, fk := runAgentSkillsFixture(t, def, rs, assets, blobs)

	if len(fk.Calls()) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(fk.Calls()))
	}
	got, ok := rs.LookupSelectedSkills("hunt")
	if !ok {
		t.Fatalf("LookupSelectedSkills(hunt) missing")
	}
	if got.Library != "awf" || got.LibraryDigest == "" || got.Router != skillroute.RouterName || got.RouterVersion != skillroute.RouterVersion {
		t.Fatalf("selected metadata = %#v", got)
	}
	if !reflect.DeepEqual(got.RouterParams, skillroute.RouterParams()) {
		t.Fatalf("RouterParams = %#v, want %#v", got.RouterParams, skillroute.RouterParams())
	}
	if len(got.Selected) != 1 || got.Selected[0].ID != "kube" || got.Selected[0].Score <= 0 {
		t.Fatalf("Selected = %#v, want one positive-score kube selection", got.Selected)
	}

	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	skillsIdx, startedIdx, completedIdx := -1, -1, -1
	for i, ev := range events {
		if ev.Path != "hunt" {
			continue
		}
		switch ev.Type {
		case engine.EventSkillsSelected:
			if skillsIdx != -1 {
				t.Fatalf("multiple skills.selected events for hunt")
			}
			skillsIdx = i
		case engine.EventNodeStarted:
			startedIdx = i
		case engine.EventNodeCompleted:
			completedIdx = i
		}
	}
	if skillsIdx == -1 || startedIdx == -1 || completedIdx == -1 {
		t.Fatalf("event indexes: skills=%d started=%d completed=%d; events=%+v", skillsIdx, startedIdx, completedIdx, events)
	}
	if skillsIdx >= startedIdx || startedIdx >= completedIdx {
		t.Fatalf("event order: skills=%d started=%d completed=%d, want skills.selected before node.started before node.completed", skillsIdx, startedIdx, completedIdx)
	}
}

func TestRunAgentStep_SkillsFreshStagesOnlySelectedSkillFiles(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), nil)
	rs := engine.NewRunState("r1", "d", nil)
	rs.Assets = assets
	log, fk, be, h := runAgentSkillsStagingFixture(t, def, rs, assets, blobs)

	if len(fk.Calls()) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(fk.Calls()))
	}
	got, err := be.CaptureFiles(context.Background(), h, []string{
		"/skills/kube/SKILL.md",
		"/skills/kube/examples/network.md",
	})
	if err != nil {
		t.Fatalf("CaptureFiles selected skill files: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("captured selected files = %d, want 2", len(got))
	}
	if string(got[0].Content) != "# Kubernetes Diagnostic\nDiagnose pod crash loops, cluster network outages, and service incidents.\n" {
		t.Fatalf("/skills/kube/SKILL.md content = %q", got[0].Content)
	}
	if string(got[1].Content) != "Inspect pod DNS, NetworkPolicy, and service routing.\n" {
		t.Fatalf("/skills/kube/examples/network.md content = %q", got[1].Content)
	}
	if _, err := be.CaptureFiles(context.Background(), h, []string{"/skills/billing/SKILL.md"}); err == nil {
		t.Fatal("CaptureFiles unselected billing skill succeeded, want absent file")
	}
	assertHasEventForPath(t, log, engine.EventSkillsSelected, "hunt")
}

func TestRunAgentStep_SkillsInputFilesCollisionFailsBeforeDispatch(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), func(wf *ir.Workflow) {
		step := wf.Graph[0].(*ir.AgentStep)
		step.InputFiles = map[string]string{"/skills/kube/SKILL.md": "asset.prompt"}
		wf.Assets["prompt"] = "prompt.txt"
	})
	promptAssets, err := engine.StoreRunStartedAssets(blobs, map[string]ir.LoadedAsset{
		"prompt": {
			ID:           "prompt",
			DeclaredPath: "prompt.txt",
			IsDir:        false,
			Files: []ir.LoadedAssetFile{
				{Path: ".", Bytes: []byte("author prompt\n"), Size: int64(len("author prompt\n"))},
			},
		},
	})
	if err != nil {
		t.Fatalf("StoreRunStartedAssets prompt: %v", err)
	}
	for id, asset := range promptAssets {
		assets[id] = asset
	}
	rs := engine.NewRunState("r1", "d", nil)
	rs.Assets = assets
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk, dispatcher := skillsDispatcher(t)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (err=%v)", oc, engine.OutcomePermanentFailure, err)
	}
	if err == nil || !strings.Contains(err.Error(), "expanded paths collide") {
		t.Fatalf("err = %v, want expanded paths collide", err)
	}
	if len(fk.Calls()) != 0 {
		t.Fatalf("fake.Calls len = %d, want 0", len(fk.Calls()))
	}
	assertHasEventForPath(t, log, engine.EventSkillsSelected, "hunt")
	assertNoEventForPath(t, log, engine.EventNodeStarted, "hunt")
}

func TestRunAgentStep_ContainerlessSkillsRejectedBeforeSelection(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), func(wf *ir.Workflow) {
		step := wf.Graph[0].(*ir.AgentStep)
		step.Container = ""
	})
	rs := engine.NewRunState("r1", "d", nil)
	rs.Assets = assets
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk, dispatcher := skillsDispatcher(t)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (err=%v)", oc, engine.OutcomePermanentFailure, err)
	}
	if err == nil || !strings.Contains(err.Error(), "skills requires a container") {
		t.Fatalf("err = %v, want skills requires a container", err)
	}
	if len(fk.Calls()) != 0 {
		t.Fatalf("fake.Calls len = %d, want 0", len(fk.Calls()))
	}
	assertNoEventForPath(t, log, engine.EventSkillsSelected, "hunt")
	assertNoEventForPath(t, log, engine.EventNodeStarted, "hunt")
}

func TestRunAgentStep_SkillsNoMatchFailsBeforeDispatch(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("galaxy nebula comet"), nil)
	rs := engine.NewRunState("r1", "d", nil)
	rs.Assets = assets

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk, dispatcher := skillsDispatcher(t)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if oc != engine.OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (err=%v)", oc, engine.OutcomePermanentFailure, err)
	}
	if err == nil || !strings.Contains(err.Error(), "no skills matched") {
		t.Fatalf("err = %v, want no skills matched", err)
	}
	if len(fk.Calls()) != 0 {
		t.Fatalf("fake.Calls len = %d, want 0", len(fk.Calls()))
	}
	assertNoEventForPath(t, log, engine.EventSkillsSelected, "hunt")
	assertNoEventForPath(t, log, engine.EventNodeStarted, "hunt")
}
