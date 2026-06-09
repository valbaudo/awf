package yaml

import (
	"errors"
	"testing"
	"time"

	goyaml "github.com/goccy/go-yaml"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/skillroute"
)

const sampleYAML = `
workflow: demo
version: 1
containers:
  lab:
    image: oci://x@sha256:abc
graph:
  - id: a
    container: lab
    run: ./a.sh
  - skip: "not applicable"
  - parallel:
      - id: b
        container: lab
        run: ./b.sh
      - id: c
        container: lab
        run: ./c.sh
`

func TestDecodeValidWorkflow(t *testing.T) {
	wf, err := Decode([]byte(sampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	if wf.ID != "demo" {
		t.Fatalf("ID = %q, want demo", wf.ID)
	}
	if wf.Version != 1 {
		t.Fatalf("Version = %d, want 1", wf.Version)
	}
	c, ok := wf.Containers["lab"]
	if !ok || c.Image != "oci://x@sha256:abc" {
		t.Fatalf("Containers[lab] = %+v", c)
	}
	if len(wf.Graph) != 3 {
		t.Fatalf("Graph len = %d, want 3", len(wf.Graph))
	}
	if cs, ok := wf.Graph[0].(*ir.CodeStep); !ok || cs.ID != "a" || cs.Run != "./a.sh" {
		t.Fatalf("Graph[0] = %#v", wf.Graph[0])
	}
	if sk, ok := wf.Graph[1].(*ir.Skip); !ok || sk.Reason != "not applicable" {
		t.Fatalf("Graph[1] = %#v (want *ir.Skip with the standard string-value form)", wf.Graph[1])
	}
	pl, ok := wf.Graph[2].(*ir.Parallel)
	if !ok || len(pl.Children) != 2 {
		t.Fatalf("Graph[2] = %#v (want *ir.Parallel array-value form, 2 children)", wf.Graph[2])
	}
	if cb, ok := pl.Children[0].(*ir.CodeStep); !ok || cb.ID != "b" {
		t.Fatalf("Parallel[0] = %#v", pl.Children[0])
	}
}

func TestDecodeSkillRouting(t *testing.T) {
	in := []byte(`
workflow: skills-demo
version: 1
assets:
  skill_assets: skills
skills:
  web:
    from: asset.skill_assets
    layout: skill_dirs
    router: bm25
containers:
  lab:
    image: oci://x@sha256:abc
graph:
  - id: route
    container: lab
    uses: awf/native
    skills:
      from: web
      query: "{{ input.target }}"
      limit: 5
      into: /work/.awf/skills
`)
	wf, err := Decode(in)
	if err != nil {
		t.Fatal(err)
	}
	corpus, ok := wf.Skills["web"]
	if !ok {
		t.Fatalf("Skills[web] missing after decode: %#v", wf.Skills)
	}
	if corpus.From != "asset.skill_assets" || corpus.Layout != skillroute.LayoutSkillDirs || corpus.Router != skillroute.RouterName {
		t.Fatalf("Skills[web] = %+v, want from/layout/router preserved", corpus)
	}
	step, ok := wf.Graph[0].(*ir.AgentStep)
	if !ok {
		t.Fatalf("Graph[0] = %#v, want *ir.AgentStep", wf.Graph[0])
	}
	if step.Skills == nil {
		t.Fatal("AgentStep.Skills is nil after decode")
	}
	if step.Skills.From != "web" || step.Skills.Query != ir.Template("{{ input.target }}") ||
		step.Skills.Limit != 5 || step.Skills.Into != "/work/.awf/skills" {
		t.Fatalf("AgentStep.Skills = %+v, want from/query/limit/into preserved", step.Skills)
	}
}

func TestDecodeYAMLSyntaxError(t *testing.T) {
	// A malformed YAML — goccy must surface a *goyaml.SyntaxError with a precise line so
	// downstream validation can report it as a position-aware diagnostic. Bare-string match
	// on err.Error() is brittle (the digit "5" appears in any path / hash incidentally); the
	// typed-error assertion is the real contract.
	bad := []byte("workflow: demo\ngraph:\n  - id: a\n    run: ./a.sh\n  bad-indent\n")
	_, err := Decode(bad)
	if err == nil {
		t.Fatal("expected parse error")
	}
	var se *goyaml.SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("expected *goyaml.SyntaxError unwrappable from %T: %v", err, err)
	}
	if se.Token == nil || se.Token.Position == nil {
		t.Fatalf("SyntaxError missing token/position: %#v", se)
	}
	if se.Token.Position.Line != 5 {
		t.Errorf("err on line %d, want 5; full err: %v", se.Token.Position.Line, err)
	}
}

func TestDecodeDurationStringForm(t *testing.T) {
	// A duration declared as "24h" in YAML must reach the IR via the JSON pipeline
	// and through ir.Duration.UnmarshalJSON's string-form branch.
	in := []byte(`
workflow: demo
version: 1
containers: {}
graph:
  - id: approve
    await: human_review
    timeout: "24h"
`)
	wf, err := Decode(in)
	if err != nil {
		t.Fatal(err)
	}
	sig, ok := wf.Graph[0].(*ir.SignalStep)
	if !ok {
		t.Fatalf("Graph[0] = %#v, want *ir.SignalStep", wf.Graph[0])
	}
	if sig.Timeout == nil {
		t.Fatal("Timeout is nil")
	}
	want := int64(24 * time.Hour)
	if int64(*sig.Timeout) != want {
		t.Fatalf("Timeout = %d ns, want %d (24h)", int64(*sig.Timeout), want)
	}
}

func TestDecodeDuplicateAssetsKeyRejected(t *testing.T) {
	_, err := Decode([]byte(`
workflow: duplicate-assets
version: 1
assets:
  schema: schema-a.json
  schema: schema-b.json
containers: {}
graph: []
`))
	if err == nil {
		t.Fatal("expected duplicate assets key to be rejected")
	}
}
