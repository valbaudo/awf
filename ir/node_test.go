package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStepRoundTrip(t *testing.T) {
	in := `{"id":"triage","container":"lab","run":"./x.sh"}`
	n, err := unmarshalNode(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	cs, ok := n.(*CodeStep)
	if !ok {
		t.Fatalf("got %T, want *CodeStep", n)
	}
	if cs.ID != "triage" || cs.Run != "./x.sh" || cs.Container != "lab" {
		t.Fatalf("bad decode: %+v", cs)
	}
	out, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round-trip: got %s want %s", out, in)
	}
}

func TestControlMarshalShape(t *testing.T) {
	b, err := json.Marshal(&Gate{Until: "x", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, `{"gate":`) {
		t.Fatalf("gate did not marshal to a wrapper object: %s", s)
	}
	if !strings.Contains(s, `"until":"x"`) || !strings.Contains(s, `"max_attempts":3`) {
		t.Fatalf("gate inner content missing: %s", s)
	}
}

func TestZeroKindKeysIsError(t *testing.T) {
	if _, err := unmarshalNode(json.RawMessage(`{"id":"x"}`)); err == nil {
		t.Fatal("expected error for node with no kind key")
	}
}

func TestMultipleKindKeysIsError(t *testing.T) {
	if _, err := unmarshalNode(json.RawMessage(`{"id":"x","run":"a","uses":"b"}`)); err == nil {
		t.Fatal("expected error for node with multiple kind keys")
	}
}

func TestNestedRoundTrip(t *testing.T) {
	in := `{"try":{"do":[{"gate":{"generate":[{"id":"g","run":"gen"}],"evaluate":[{"id":"e","run":"ev"}],"until":"x","max_attempts":3}}]}}`
	n, err := unmarshalNode(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	tryN, ok := n.(*Try)
	if !ok {
		t.Fatalf("got %T, want *Try", n)
	}
	gateN, ok := tryN.Do[0].(*Gate)
	if !ok {
		t.Fatalf("try.do[0] = %T, want *Gate", tryN.Do[0])
	}
	if _, ok := gateN.Generate[0].(*CodeStep); !ok {
		t.Fatalf("gate.generate[0] = %T, want *CodeStep", gateN.Generate[0])
	}
	// re-marshal and decode again: structure must survive a round-trip
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := unmarshalNode(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := n2.(*Try).Do[0].(*Gate).Generate[0].(*CodeStep); !ok {
		t.Fatalf("structure lost on round-trip: %s", out)
	}
}

func TestGraphUnmarshal(t *testing.T) {
	in := `{"workflow":"w","version":1,"containers":{},"graph":[{"id":"a","run":"x"},{"skip":"done"}]}`
	var wf Workflow
	if err := json.Unmarshal([]byte(in), &wf); err != nil {
		t.Fatal(err)
	}
	if len(wf.Graph) != 2 {
		t.Fatalf("graph len = %d", len(wf.Graph))
	}
	if _, ok := wf.Graph[0].(*CodeStep); !ok {
		t.Fatalf("graph[0] = %T", wf.Graph[0])
	}
	if _, ok := wf.Graph[1].(*Skip); !ok {
		t.Fatalf("graph[1] = %T", wf.Graph[1])
	}
}

func TestSkipSurface(t *testing.T) {
	// Marshal: standard §5.6 string-value form.
	b, err := json.Marshal(&Skip{Reason: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"skip":"done"}` {
		t.Fatalf("Skip marshal = %s, want {\"skip\":\"done\"}", b)
	}
	// Decode: string form.
	n, err := unmarshalNode(json.RawMessage(`{"skip":"done"}`))
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := n.(*Skip); !ok || s.Reason != "done" {
		t.Fatalf("decode: got %#v", n)
	}
	// Decode: null reason (optional <reason>) → empty Reason, no error.
	n2, err := unmarshalNode(json.RawMessage(`{"skip":null}`))
	if err != nil || n2.(*Skip).Reason != "" {
		t.Fatalf("null reason: n=%#v err=%v", n2, err)
	}
	// Regression guard: the OLD object-form must be rejected.
	if _, err := unmarshalNode(json.RawMessage(`{"skip":{"reason":"x"}}`)); err == nil {
		t.Fatal("old object-form {\"skip\":{\"reason\":...}} must error")
	}
}

func TestParallelSurface(t *testing.T) {
	// Marshal: standard §5.4 array-value form.
	b, err := json.Marshal(&Parallel{Children: NodeList{&CodeStep{ID: "a", Run: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"parallel":[{"id":"a","run":"x"}]}` {
		t.Fatalf("Parallel marshal = %s, want {\"parallel\":[{\"id\":\"a\",\"run\":\"x\"}]}", b)
	}
	// Decode: array form.
	n, err := unmarshalNode(json.RawMessage(`{"parallel":[{"id":"a","run":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := n.(*Parallel)
	if !ok || len(p.Children) != 1 {
		t.Fatalf("decode: got %#v", n)
	}
	if _, ok := p.Children[0].(*CodeStep); !ok {
		t.Fatalf("parallel.children[0] = %T, want *CodeStep", p.Children[0])
	}
}

func TestMapMinSuccessAcceptsNumber(t *testing.T) {
	// Regression for the Ratio fix: min_success may be a count (3) or a fraction (0.8).
	in := `{"map":{"over":"o","as":"i","container":"c","concurrency":1,"min_success":3,"body":[]}}`
	n, err := unmarshalNode(json.RawMessage(in))
	if err != nil {
		t.Fatalf("numeric min_success must decode: %v", err)
	}
	m, ok := n.(*Map)
	if !ok {
		t.Fatalf("got %T, want *Map", n)
	}
	if m.MinSuccess == nil || string(*m.MinSuccess) != "3" {
		t.Fatalf("min_success = %v", m.MinSuccess)
	}
	// Marshal direction: the value must emit as a bare JSON number, not a quoted string.
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"min_success":3`) {
		t.Fatalf("min_success did not emit as a bare number: %s", out)
	}
}

func TestAgentStepRoundTrip(t *testing.T) {
	in := `{"id":"triage","container":"lab","uses":"anthropic/claude-code","with":{"skill":"cve-triage"}}`
	n, err := unmarshalNode(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	a, ok := n.(*AgentStep)
	if !ok {
		t.Fatalf("got %T, want *AgentStep", n)
	}
	if a.ID != "triage" || a.Uses != "anthropic/claude-code" || a.With["skill"] != "cve-triage" {
		t.Fatalf("bad decode: %+v", a)
	}
	out, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round-trip: got %s want %s", out, in)
	}
}

func TestSignalStepRoundTrip(t *testing.T) {
	in := `{"id":"approve","await":"human_review"}`
	n, err := unmarshalNode(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := n.(*SignalStep)
	if !ok {
		t.Fatalf("got %T, want *SignalStep", n)
	}
	if s.ID != "approve" || s.Await != "human_review" {
		t.Fatalf("bad decode: %+v", s)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round-trip: got %s want %s", out, in)
	}
}

func TestIfRoundTrip(t *testing.T) {
	in := `{"if":{"cond":"{{ step.x.ok }}","then":[{"id":"a","run":"x"}]}}`
	n, err := unmarshalNode(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	ifN, ok := n.(*If)
	if !ok {
		t.Fatalf("got %T, want *If", n)
	}
	if len(ifN.Then) != 1 {
		t.Fatalf("then len = %d", len(ifN.Then))
	}
	if _, ok := ifN.Then[0].(*CodeStep); !ok {
		t.Fatalf("if.then[0] = %T, want *CodeStep", ifN.Then[0])
	}
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round-trip: got %s want %s", out, in)
	}
}

func TestLoopRoundTrip(t *testing.T) {
	in := `{"loop":{"until":"{{ step.x.done }}","body":[{"id":"a","run":"x"}]}}`
	n, err := unmarshalNode(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	loopN, ok := n.(*Loop)
	if !ok {
		t.Fatalf("got %T, want *Loop", n)
	}
	if loopN.Until == nil || string(*loopN.Until) != `{{ step.x.done }}` {
		t.Fatalf("until = %v", loopN.Until)
	}
	if len(loopN.Body) != 1 {
		t.Fatalf("body len = %d", len(loopN.Body))
	}
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round-trip: got %s want %s", out, in)
	}
}

func TestIfElseRoundTrip(t *testing.T) {
	in := `{"if":{"cond":"x","then":[{"id":"a","run":"a"}],"else":[{"id":"b","run":"b"}]}}`
	n, err := unmarshalNode(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	ifN := n.(*If)
	if len(ifN.Else) != 1 {
		t.Fatalf("else len = %d, want 1", len(ifN.Else))
	}
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round-trip: got %s want %s", out, in)
	}
}

func TestLoopMaxItersRoundTrip(t *testing.T) {
	in := `{"loop":{"max_iters":5,"body":[{"id":"a","run":"x"}]}}`
	n, err := unmarshalNode(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	lp := n.(*Loop)
	if lp.MaxIters == nil || *lp.MaxIters != 5 {
		t.Fatalf("max_iters = %v, want 5", lp.MaxIters)
	}
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round-trip: got %s want %s", out, in)
	}
}

func TestTryCatchFinallyRoundTrip(t *testing.T) {
	in := `{"try":{"do":[{"id":"d","run":"d"}],"catch":[{"id":"c","run":"c"}],"finally":[{"id":"f","run":"f"}]}}`
	n, err := unmarshalNode(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	try := n.(*Try)
	if len(try.Catch) != 1 || len(try.Finally) != 1 {
		t.Fatalf("catch/finally len: catch=%d finally=%d", len(try.Catch), len(try.Finally))
	}
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round-trip: got %s want %s", out, in)
	}
}

// TestNodeRegistryExhaustive guards the 3-touchpoint invariant called out in node.go: every
// controlKeys entry must have a matching case in unmarshalControl, and the total number of kinds
// (control + step) must equal the count of concrete Node types. A future contributor who adds a
// control type to the factory but forgets the switch case lands in the "unknown control" branch,
// which this test catches before it can ship.
func TestNodeRegistryExhaustive(t *testing.T) {
	const wantKinds = 10 // 3 step + 7 control; update when (the standard's set of) node kinds changes.
	if got := len(controlKeys) + len(stepKeys); got != wantKinds {
		t.Fatalf("registries cover %d kinds, want %d", got, wantKinds)
	}
	for k, mk := range controlKeys {
		n := mk()
		if err := unmarshalControl(k, json.RawMessage(`null`), n); err != nil {
			if strings.Contains(err.Error(), "unknown control") {
				t.Errorf("unmarshalControl(%q): %v — missing case in the switch", k, err)
			}
		}
	}
}
