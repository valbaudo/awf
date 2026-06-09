package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/skillroute"
	"github.com/valbaudo/awf/state"
)

func TestSkillsSelectedData_JSONRoundTrip(t *testing.T) {
	in := engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: "sha256:abc",
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  map[string]float64{"k1": 1.2, "b": 0.75},
		Selected:      []engine.SelectedSkill{{ID: "kube", Score: 3.25}, {ID: "billing", Score: 1.5}},
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{
		`"library":"awf"`,
		`"library_digest":"sha256:abc"`,
		`"router":"bm25"`,
		`"router_version":"bm25-weighted-v1"`,
		`"router_params"`,
		`"selected"`,
		`"id":"kube"`,
		`"score":3.25`,
	} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("skills.selected JSON = %s, want substring %q", b, want)
		}
	}

	var got engine.SkillsSelectedData
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round-trip mismatch:\n got: %#v\nwant: %#v", got, in)
	}
}

func TestFold_SkillsSelectedRecordsRunState(t *testing.T) {
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	want := engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: "sha256:abc",
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected:      []engine.SelectedSkill{{ID: "kube", Score: 4.5}},
	}
	if err := log.Append(state.Event{Type: engine.EventSkillsSelected, Path: "hunt", Data: mustJSON(want)}); err != nil {
		t.Fatalf("append skills.selected: %v", err)
	}
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}

	rs, err := engine.Fold(events, state.NewInMemoryBlobs())
	if err != nil {
		t.Fatalf("engine.Fold: %v", err)
	}
	got, ok := rs.LookupSelectedSkills("hunt")
	if !ok {
		t.Fatalf("LookupSelectedSkills(hunt) missing")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("selected skills = %#v, want %#v", got, want)
	}
}

func TestFold_SkillsSelectedMalformedDataIsError(t *testing.T) {
	events := []state.Event{
		{Seq: 1, Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})},
		{Seq: 2, Type: engine.EventSkillsSelected, Path: "hunt", Data: []byte(`{`)},
	}
	_, err := engine.Fold(events, state.NewInMemoryBlobs())
	if err == nil {
		t.Fatal("engine.Fold succeeded, want malformed skills.selected error")
	}
	if !strings.Contains(err.Error(), "skills.selected") {
		t.Fatalf("error = %v, want skills.selected context", err)
	}
}

func TestFold_SkillsSelectedRejectsEmptySelectionBeforeCompletedShortCircuit(t *testing.T) {
	base := engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: "sha256:abc",
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
	}

	tests := []struct {
		name string
		data engine.SkillsSelectedData
	}{
		{name: "nil selected", data: base},
		{name: "empty selected", data: func() engine.SkillsSelectedData {
			d := base
			d.Selected = []engine.SelectedSkill{}
			return d
		}()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := []state.Event{
				{Seq: 1, Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})},
				{Seq: 2, Type: engine.EventSkillsSelected, Path: "hunt", Data: mustJSON(tc.data)},
				{Seq: 3, Type: engine.EventNodeCompleted, Path: "hunt", Data: mustJSON(engine.NodeCompletedData{Outcome: string(engine.OutcomeOK)})},
			}
			_, err := engine.Fold(events, state.NewInMemoryBlobs())
			if err == nil {
				t.Fatal("engine.Fold succeeded, want empty skills.selected selection error")
			}
			for _, term := range []string{"skills.selected", "selected", "empty"} {
				if !strings.Contains(err.Error(), term) {
					t.Fatalf("error = %v, want term %q", err, term)
				}
			}
		})
	}
}

func TestFold_SkillsSelectedRejectsInvalidSelectionBeforeCompletedShortCircuit(t *testing.T) {
	base := engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: "sha256:abc",
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected:      []engine.SelectedSkill{{ID: "kube", Score: 4.5}},
	}
	withData := func(d engine.SkillsSelectedData) []byte {
		return mustJSON(d)
	}

	tests := []struct {
		name      string
		data      []byte
		wantTerms []string
	}{
		{
			name: "duplicate selected ids",
			data: withData(func() engine.SkillsSelectedData {
				d := base
				d.Selected = []engine.SelectedSkill{{ID: "kube", Score: 4.5}, {ID: "kube", Score: 3.25}}
				return d
			}()),
			wantTerms: []string{"skills.selected", "duplicate"},
		},
		{
			name: "zero score",
			data: withData(func() engine.SkillsSelectedData {
				d := base
				d.Selected = []engine.SelectedSkill{{ID: "kube", Score: 0}}
				return d
			}()),
			wantTerms: []string{"skills.selected", "score"},
		},
		{
			name: "negative score",
			data: withData(func() engine.SkillsSelectedData {
				d := base
				d.Selected = []engine.SelectedSkill{{ID: "kube", Score: -0.5}}
				return d
			}()),
			wantTerms: []string{"skills.selected", "score"},
		},
		{
			name: "empty selected id",
			data: withData(func() engine.SkillsSelectedData {
				d := base
				d.Selected = []engine.SelectedSkill{{ID: "", Score: 4.5}}
				return d
			}()),
			wantTerms: []string{"skills.selected", "id"},
		},
		{
			name: "missing library",
			data: withData(func() engine.SkillsSelectedData {
				d := base
				d.Library = ""
				return d
			}()),
			wantTerms: []string{"skills.selected", "library"},
		},
		{
			name: "missing library digest",
			data: withData(func() engine.SkillsSelectedData {
				d := base
				d.LibraryDigest = ""
				return d
			}()),
			wantTerms: []string{"skills.selected", "library_digest"},
		},
		{
			name: "missing router",
			data: withData(func() engine.SkillsSelectedData {
				d := base
				d.Router = ""
				return d
			}()),
			wantTerms: []string{"skills.selected", "router"},
		},
		{
			name: "missing router version",
			data: withData(func() engine.SkillsSelectedData {
				d := base
				d.RouterVersion = ""
				return d
			}()),
			wantTerms: []string{"skills.selected", "router_version"},
		},
		{
			name:      "non-finite score token",
			data:      []byte(`{"library":"awf","library_digest":"sha256:abc","router":"bm25","router_version":"bm25-weighted-v1","router_params":{},"selected":[{"id":"kube","score":NaN}]}`),
			wantTerms: []string{"skills.selected"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := []state.Event{
				{Seq: 1, Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})},
				{Seq: 2, Type: engine.EventSkillsSelected, Path: "hunt", Data: tc.data},
				{Seq: 3, Type: engine.EventNodeCompleted, Path: "hunt", Data: mustJSON(engine.NodeCompletedData{Outcome: string(engine.OutcomeOK)})},
			}
			_, err := engine.Fold(events, state.NewInMemoryBlobs())
			if err == nil {
				t.Fatal("engine.Fold succeeded, want invalid skills.selected selection error")
			}
			for _, term := range tc.wantTerms {
				if !strings.Contains(err.Error(), term) {
					t.Fatalf("error = %v, want term %q", err, term)
				}
			}
		})
	}
}

func TestFold_SkillsSelectedRejectsDuplicateAndEmptyPath(t *testing.T) {
	base := engine.SkillsSelectedData{
		Library:       "awf",
		LibraryDigest: "sha256:abc",
		Router:        skillroute.RouterName,
		RouterVersion: skillroute.RouterVersion,
		RouterParams:  skillroute.RouterParams(),
		Selected:      []engine.SelectedSkill{{ID: "kube", Score: 4.5}},
	}
	other := base
	other.Selected = []engine.SelectedSkill{{ID: "billing", Score: 3.25}}

	tests := []struct {
		name      string
		events    []state.Event
		wantTerms []string
	}{
		{
			name: "duplicate path",
			events: []state.Event{
				{Seq: 1, Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})},
				{Seq: 2, Type: engine.EventSkillsSelected, Path: "hunt", Data: mustJSON(base)},
				{Seq: 3, Type: engine.EventSkillsSelected, Path: "hunt", Data: mustJSON(other)},
			},
			wantTerms: []string{"skills.selected", "already recorded"},
		},
		{
			name: "empty path",
			events: []state.Event{
				{Seq: 1, Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d"})},
				{Seq: 2, Type: engine.EventSkillsSelected, Data: mustJSON(base)},
			},
			wantTerms: []string{"skills.selected", "path"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := engine.Fold(tc.events, state.NewInMemoryBlobs())
			if err == nil {
				t.Fatal("engine.Fold succeeded, want skills.selected fold error")
			}
			for _, term := range tc.wantTerms {
				if !strings.Contains(err.Error(), term) {
					t.Fatalf("error = %v, want term %q", err, term)
				}
			}
		})
	}
}

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

func skillWorkflowFixture(t *testing.T, query ir.Template, mutate func(*ir.Workflow)) (*ir.LoadedDefinition, map[string]engine.RunStartedAsset, state.Blobs) {
	t.Helper()
	blobs := state.NewInMemoryBlobs()
	assets := storeTestSkillAssets(t, blobs)
	wf := &ir.Workflow{
		ID:      "skills-agent",
		Version: 1,
		Assets:  map[string]string{"skill_assets": "skills"},
		Skills: map[string]ir.SkillCorpus{
			"awf": {From: "asset.skill_assets", Layout: "skill_dirs", Router: skillroute.RouterName},
		},
		Containers: map[string]ir.Container{"lab": {}},
		Graph: ir.NodeList{
			&ir.AgentStep{
				ID:        "hunt",
				Container: "lab",
				Uses:      "anthropic/claude-code",
				With:      ir.RawConfig{"prompt": "go"},
				Skills:    &ir.StepSkillRouting{From: "awf", Query: query, Limit: 1, Into: "/skills"},
				OutputSchema: &ir.JSONSchema{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"ok"},
					"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
				},
			},
		},
	}
	if mutate != nil {
		mutate(wf)
	}
	return &ir.LoadedDefinition{Workflow: wf}, assets, blobs
}

func runAgentSkillsFixture(t *testing.T, def *ir.LoadedDefinition, rs *engine.RunState, assets map[string]engine.RunStartedAsset, blobs state.Blobs) (*state.InMemoryLog, *fake.Fake) {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk := runAgentWithState(t, def, rs, log, blobs, assets)
	return log, fk
}

func runAgentSkillsStagingFixture(t *testing.T, def *ir.LoadedDefinition, rs *engine.RunState, assets map[string]engine.RunStartedAsset, blobs state.Blobs) (*state.InMemoryLog, *fake.Fake, *container.Fake, container.Handle) {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := state.NewInMemoryLog(clk)
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: mustJSON(engine.RunStartedData{RunID: "r1", WorkflowDigest: "d", Assets: assets})}); err != nil {
		t.Fatalf("append run.started: %v", err)
	}
	fk, be, h := runAgentWithStateAndContainer(t, def, rs, log, blobs, assets)
	return log, fk, be, h
}

func runAgentWithState(t *testing.T, def *ir.LoadedDefinition, rs *engine.RunState, log *state.InMemoryLog, blobs state.Blobs, assets map[string]engine.RunStartedAsset) *fake.Fake {
	t.Helper()
	fk, _, _ := runAgentWithStateAndContainer(t, def, rs, log, blobs, assets)
	return fk
}

func runAgentWithStateAndContainer(t *testing.T, def *ir.LoadedDefinition, rs *engine.RunState, log *state.InMemoryLog, blobs state.Blobs, assets map[string]engine.RunStartedAsset) (*fake.Fake, *container.Fake, container.Handle) {
	t.Helper()
	fk, be, h, dispatcher := skillsDispatcherWithContainer(t)
	clk := &clock.Fake{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	oc, err := engine.Run(context.Background(), def, rs, dispatcher, log, blobs, clk, engine.RunOptions{Tap: io.Discard, Assets: assets})
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, engine.OutcomeOK)
	}
	return fk, be, h
}

func foldedSkillsSelectedReplayState(t *testing.T, assets map[string]engine.RunStartedAsset, blobs state.Blobs, recorded engine.SkillsSelectedData) (*state.InMemoryLog, *engine.RunState) {
	t.Helper()
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
	return log, rs
}

func skillsDispatcher(t *testing.T) (*fake.Fake, engine.Dispatcher) {
	t.Helper()
	fk, _, _, dispatcher := skillsDispatcherWithContainer(t)
	return fk, dispatcher
}

func skillsDispatcherWithContainer(t *testing.T) (*fake.Fake, *container.Fake, container.Handle, engine.Dispatcher) {
	t.Helper()
	var reg agent.Registry
	fk := fake.New("anthropic/claude-code").Script(0, fake.Result{Output: map[string]any{"ok": true}})
	if err := reg.Register(fk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	be := container.NewFake()
	h, err := be.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	if err != nil {
		t.Fatalf("Create lab: %v", err)
	}
	return fk, be, h, &engine.LocalDispatcher{
		Backend:  be,
		Handles:  map[string]container.Handle{"lab": h},
		Resolver: &reg,
	}
}

func storeTestSkillAssets(t *testing.T, blobs state.Blobs) map[string]engine.RunStartedAsset {
	t.Helper()
	assets, err := engine.StoreRunStartedAssets(blobs, map[string]ir.LoadedAsset{
		"skill_assets": {
			ID:           "skill_assets",
			DeclaredPath: "skills",
			IsDir:        true,
			Files: []ir.LoadedAssetFile{
				{Path: "billing/SKILL.md", Bytes: []byte("# Billing Helper\nReconcile invoices, payments, taxes, and customer charges.\n"), Size: int64(len("# Billing Helper\nReconcile invoices, payments, taxes, and customer charges.\n"))},
				{Path: "billing/examples/taxes.md", Bytes: []byte("Review tax rules, invoice line items, and customer balances.\n"), Size: int64(len("Review tax rules, invoice line items, and customer balances.\n"))},
				{Path: "kube/SKILL.md", Bytes: []byte("# Kubernetes Diagnostic\nDiagnose pod crash loops, cluster network outages, and service incidents.\n"), Size: int64(len("# Kubernetes Diagnostic\nDiagnose pod crash loops, cluster network outages, and service incidents.\n"))},
				{Path: "kube/examples/network.md", Bytes: []byte("Inspect pod DNS, NetworkPolicy, and service routing.\n"), Size: int64(len("Inspect pod DNS, NetworkPolicy, and service routing.\n"))},
			},
		},
	})
	if err != nil {
		t.Fatalf("StoreRunStartedAssets: %v", err)
	}
	return assets
}

func testSkillCorpusDigest(t *testing.T) string {
	t.Helper()
	corpus, err := skillroute.NewCorpus("awf", []skillroute.File{
		{Path: "billing/SKILL.md", Content: []byte("# Billing Helper\nReconcile invoices, payments, taxes, and customer charges.\n")},
		{Path: "billing/examples/taxes.md", Content: []byte("Review tax rules, invoice line items, and customer balances.\n")},
		{Path: "kube/SKILL.md", Content: []byte("# Kubernetes Diagnostic\nDiagnose pod crash loops, cluster network outages, and service incidents.\n")},
		{Path: "kube/examples/network.md", Content: []byte("Inspect pod DNS, NetworkPolicy, and service routing.\n")},
	})
	if err != nil {
		t.Fatalf("skillroute.NewCorpus: %v", err)
	}
	return corpus.Digest()
}

func assertNoEventForPath(t *testing.T, log *state.InMemoryLog, eventType, path string) {
	t.Helper()
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	for _, ev := range events {
		if ev.Type == eventType && ev.Path == path {
			t.Fatalf("found unexpected %s at %q: %+v", eventType, path, ev)
		}
	}
}

func assertHasEventForPath(t *testing.T, log *state.InMemoryLog, eventType, path string) {
	t.Helper()
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	for _, ev := range events {
		if ev.Type == eventType && ev.Path == path {
			return
		}
	}
	t.Fatalf("missing expected %s at %q", eventType, path)
}
