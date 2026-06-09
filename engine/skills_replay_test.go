package engine_test

import (
	"context"
	"io"
	"math"
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

func TestRunAgentStep_SkillsReplayUsesRecordedSelection(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("billing invoices taxes"), nil)
	digest := testSkillCorpusDigest(t)
	recorded := engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: digest,
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected:      []engine.SelectedSkill{{ID: "kube", Score: 99}},
	}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventSkillsSelected, Path: "hunt", Data: mustJSON(recorded)}); err != nil {
		t.Fatalf("append skills.selected: %v", err)
	}
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	rs, err := engine.Fold(events, blobs)
	if err != nil {
		t.Fatalf("engine.Fold: %v", err)
	}

	fk := runAgentWithState(t, def, rs, log, blobs, assets)
	if len(fk.Calls()) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(fk.Calls()))
	}

	after, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold after run: %v", err)
	}
	count := 0
	for _, ev := range after {
		if ev.Type == engine.EventSkillsSelected && ev.Path == "hunt" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("skills.selected count after replay = %d, want 1", count)
	}
	got, ok := rs.LookupSelectedSkills("hunt")
	if !ok {
		t.Fatalf("LookupSelectedSkills(hunt) missing")
	}
	if !reflect.DeepEqual(got, recorded) {
		t.Fatalf("recorded selection changed:\n got: %#v\nwant: %#v", got, recorded)
	}
}

func TestRunAgentStep_SkillsReplayStagesRecordedSelection(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("billing invoices taxes"), nil)
	recorded := engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: testSkillCorpusDigest(t),
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected:      []engine.SelectedSkill{{ID: "kube", Score: 99}},
	}
	log, rs := foldedSkillsSelectedReplayState(t, assets, blobs, recorded)

	fk, be, h := runAgentWithStateAndContainer(t, def, rs, log, blobs, assets)
	if len(fk.Calls()) != 1 {
		t.Fatalf("fake.Calls len = %d, want 1", len(fk.Calls()))
	}
	if _, err := be.CaptureFiles(context.Background(), h, []string{"/skills/kube/SKILL.md"}); err != nil {
		t.Fatalf("CaptureFiles recorded kube skill: %v", err)
	}
	if _, err := be.CaptureFiles(context.Background(), h, []string{"/skills/billing/SKILL.md"}); err == nil {
		t.Fatal("CaptureFiles current-query billing skill succeeded, want absent file")
	}
	after, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold after run: %v", err)
	}
	count := 0
	for _, ev := range after {
		if ev.Type == engine.EventSkillsSelected && ev.Path == "hunt" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("skills.selected count after replay = %d, want 1", count)
	}
}

func TestRunAgentStep_SkillsReplayMetadataMismatchIsInternal(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), nil)
	recorded := engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: "sha256:not-the-recorded-corpus",
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected:      []engine.SelectedSkill{{ID: "kube", Score: 99}},
	}

	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventSkillsSelected, Path: "hunt", Data: mustJSON(recorded)}); err != nil {
		t.Fatalf("append skills.selected: %v", err)
	}
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	rs, err := engine.Fold(events, blobs)
	if err != nil {
		t.Fatalf("engine.Fold: %v", err)
	}
	fk, dispatcher := skillsDispatcher(t)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty internal-error outcome (err=%v)", oc, err)
	}
	if err == nil || !strings.Contains(err.Error(), "skills.selected metadata mismatch") {
		t.Fatalf("err = %v, want metadata mismatch", err)
	}
	if len(fk.Calls()) != 0 {
		t.Fatalf("fake.Calls len = %d, want 0", len(fk.Calls()))
	}
	assertNoEventForPath(t, log, engine.EventNodeStarted, "hunt")
	assertNoEventForPath(t, log, engine.EventNodeFailed, "hunt")
}

func TestRunAgentStep_SkillsReplayRejectsEmptySelection(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), nil)
	rs := engine.NewRunState("r1", "d", nil)
	rs.Assets = assets
	rs.RecordSelectedSkills("hunt", engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: testSkillCorpusDigest(t),
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected:      nil,
	})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk, dispatcher := skillsDispatcher(t)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty internal-error outcome (err=%v)", oc, err)
	}
	if err == nil || !strings.Contains(err.Error(), "invalid skills.selected selection") {
		t.Fatalf("err = %v, want invalid skills.selected selection", err)
	}
	if len(fk.Calls()) != 0 {
		t.Fatalf("fake.Calls len = %d, want 0", len(fk.Calls()))
	}
	assertNoEventForPath(t, log, engine.EventNodeStarted, "hunt")
	assertNoEventForPath(t, log, engine.EventNodeFailed, "hunt")
}

func TestRunAgentStep_SkillsReplayRejectsUnknownSelectedID(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), nil)
	recorded := engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: testSkillCorpusDigest(t),
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected:      []engine.SelectedSkill{{ID: "not-in-corpus", Score: 1}},
	}
	log, rs := foldedSkillsSelectedReplayState(t, assets, blobs, recorded)
	fk, dispatcher := skillsDispatcher(t)
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty internal-error outcome (err=%v)", oc, err)
	}
	if err == nil || !strings.Contains(err.Error(), "invalid skills.selected selection") {
		t.Fatalf("err = %v, want invalid skills.selected selection", err)
	}
	if len(fk.Calls()) != 0 {
		t.Fatalf("fake.Calls len = %d, want 0", len(fk.Calls()))
	}
	assertNoEventForPath(t, log, engine.EventNodeStarted, "hunt")
	assertNoEventForPath(t, log, engine.EventNodeFailed, "hunt")
}

func TestRunAgentStep_SkillsReplayRejectsNonFiniteScore(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), nil)
	rs := engine.NewRunState("r1", "d", nil)
	rs.Assets = assets
	rs.RecordSelectedSkills("hunt", engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: testSkillCorpusDigest(t),
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected:      []engine.SelectedSkill{{ID: "kube", Score: math.Inf(1)}},
	})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk, dispatcher := skillsDispatcher(t)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty internal-error outcome (err=%v)", oc, err)
	}
	if err == nil || !strings.Contains(err.Error(), "invalid skills.selected selection") {
		t.Fatalf("err = %v, want invalid skills.selected selection", err)
	}
	if len(fk.Calls()) != 0 {
		t.Fatalf("fake.Calls len = %d, want 0", len(fk.Calls()))
	}
	assertNoEventForPath(t, log, engine.EventNodeStarted, "hunt")
	assertNoEventForPath(t, log, engine.EventNodeFailed, "hunt")
}

func TestRunAgentStep_SkillsReplayRejectsDuplicateSelectedIDs(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), nil)
	rs := engine.NewRunState("r1", "d", nil)
	rs.Assets = assets
	rs.RecordSelectedSkills("hunt", engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: testSkillCorpusDigest(t),
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected: []engine.SelectedSkill{
			{ID: "kube", Score: 1},
			{ID: "kube", Score: 0.5},
		},
	})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk, dispatcher := skillsDispatcher(t)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty internal-error outcome (err=%v)", oc, err)
	}
	if err == nil || !strings.Contains(err.Error(), "invalid skills.selected selection") {
		t.Fatalf("err = %v, want invalid skills.selected selection", err)
	}
	if len(fk.Calls()) != 0 {
		t.Fatalf("fake.Calls len = %d, want 0", len(fk.Calls()))
	}
	assertNoEventForPath(t, log, engine.EventNodeStarted, "hunt")
	assertNoEventForPath(t, log, engine.EventNodeFailed, "hunt")
}

func TestRunAgentStep_SkillsReplayRejectsZeroScore(t *testing.T) {
	def, assets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), nil)
	rs := engine.NewRunState("r1", "d", nil)
	rs.Assets = assets
	rs.RecordSelectedSkills("hunt", engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: testSkillCorpusDigest(t),
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected:      []engine.SelectedSkill{{ID: "kube", Score: 0}},
	})
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk, dispatcher := skillsDispatcher(t)

	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty internal-error outcome (err=%v)", oc, err)
	}
	if err == nil || !strings.Contains(err.Error(), "invalid skills.selected selection") {
		t.Fatalf("err = %v, want invalid skills.selected selection", err)
	}
	if len(fk.Calls()) != 0 {
		t.Fatalf("fake.Calls len = %d, want 0", len(fk.Calls()))
	}
	assertNoEventForPath(t, log, engine.EventNodeStarted, "hunt")
	assertNoEventForPath(t, log, engine.EventNodeFailed, "hunt")
}
