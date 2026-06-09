package ir

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWorkflowAssetsJSONRoundTrip(t *testing.T) {
	raw := []byte(`{"workflow":"asset-demo","version":1,"assets":{"schema":"schemas/in.json"},"containers":{},"graph":[]}`)
	var wf Workflow
	if err := json.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if got := wf.Assets["schema"]; got != "schemas/in.json" {
		t.Fatalf("Assets[schema] = %q, want schemas/in.json", got)
	}
	out, err := json.Marshal(&wf)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) || !containsJSONField(out, "assets") {
		t.Fatalf("marshaled workflow missing assets field: %s", out)
	}
}

func TestWorkflowSkillsRoundTrip(t *testing.T) {
	wf := &Workflow{
		ID:      "skills-demo",
		Version: 1,
		Assets:  map[string]string{"skill_assets": "skills"},
		Skills: map[string]SkillCorpus{
			"web": {From: "asset.skill_assets", Layout: "skill_dirs", Router: "bm25"},
		},
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID:        "route",
				Container: "lab",
				Uses:      "awf/native",
				Skills: &StepSkillRouting{
					From:  "web",
					Query: Template("{{ input.target }}"),
					Limit: 5,
					Into:  "/work/.awf/skills",
				},
			},
		},
	}

	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	var got Workflow
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	corpus, ok := got.Skills["web"]
	if !ok {
		t.Fatalf("Skills[web] missing after round-trip: %#v", got.Skills)
	}
	if corpus.From != "asset.skill_assets" || corpus.Layout != "skill_dirs" || corpus.Router != "bm25" {
		t.Fatalf("Skills[web] = %+v, want from/layout/router preserved", corpus)
	}
	step, ok := got.Graph[0].(*AgentStep)
	if !ok {
		t.Fatalf("Graph[0] = %#v, want *AgentStep", got.Graph[0])
	}
	if step.Skills == nil {
		t.Fatal("AgentStep.Skills is nil after round-trip")
	}
	if step.Skills.From != "web" || step.Skills.Query != Template("{{ input.target }}") ||
		step.Skills.Limit != 5 || step.Skills.Into != "/work/.awf/skills" {
		t.Fatalf("AgentStep.Skills = %+v, want from/query/limit/into preserved", step.Skills)
	}
}

func containsJSONField(b []byte, name string) bool {
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return false
	}
	_, ok := obj[name]
	return ok
}

func TestDurationRoundTrip(t *testing.T) {
	// Integer ns marshal: bare integer, no quotes.
	d := Duration(5 * time.Second)
	b, err := json.Marshal(&d)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "5000000000" {
		t.Fatalf("marshal: got %s, want 5000000000", b)
	}
	// Round-trip restores the value.
	var d2 Duration
	if err := json.Unmarshal(b, &d2); err != nil {
		t.Fatal(err)
	}
	if d2 != d {
		t.Fatalf("round-trip: got %v, want %v", time.Duration(d2), time.Duration(d))
	}
}

func TestDurationParsesGoString(t *testing.T) {
	// Authors writing `timeout: 24h` in YAML reach decode as a quoted string; accept it.
	var d Duration
	if err := json.Unmarshal([]byte(`"5s"`), &d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(d) != 5*time.Second {
		t.Fatalf("got %v, want 5s", time.Duration(d))
	}
}
