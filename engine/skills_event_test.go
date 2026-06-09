package engine_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
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
