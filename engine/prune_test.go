package engine

import (
	"errors"
	"reflect"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// TestPruneControllerKeepTopKIncremental pins the incremental keep-top(k)
// behavior: nothing is pruned while <= k items have committed; once a (k+1)th
// item commits, the lowest-scoring item becomes a loser.
func TestPruneControllerKeepTopKIncremental(t *testing.T) {
	pc := newPruneController(&ir.Prune{Score: "score", Keep: &ir.PruneKeep{K: 2}})

	// item 0 (0.1): 1 <= k=2 → nothing pruned.
	d, err := pc.Report(0, float64(0.1))
	if err != nil {
		t.Fatalf("Report(0): %v", err)
	}
	if d.StopAll || len(d.PruneLosers) != 0 {
		t.Fatalf("after item 0: got %+v, want no losers", d)
	}

	// item 1 (0.9): 2 <= k=2 → nothing pruned.
	d, err = pc.Report(1, float64(0.9))
	if err != nil {
		t.Fatalf("Report(1): %v", err)
	}
	if d.StopAll || len(d.PruneLosers) != 0 {
		t.Fatalf("after item 1: got %+v, want no losers", d)
	}

	// item 2 (0.5): 3 > k=2 → the lowest (item 0, 0.1) is now a loser.
	d, err = pc.Report(2, float64(0.5))
	if err != nil {
		t.Fatalf("Report(2): %v", err)
	}
	if d.StopAll || !reflect.DeepEqual(d.PruneLosers, []int{0}) {
		t.Fatalf("after item 2: got %+v, want losers {0}", d)
	}

	// item 3 (0.7): 4 > k=2 → losers are the two lowest, {0, 2}.
	d, err = pc.Report(3, float64(0.7))
	if err != nil {
		t.Fatalf("Report(3): %v", err)
	}
	if d.StopAll || !reflect.DeepEqual(d.PruneLosers, []int{0, 2}) {
		t.Fatalf("after item 3: got %+v, want losers {0,2}", d)
	}
}

// TestPruneControllerKeepTopKTieBreak pins the deterministic tie-break: among
// equal scores, the LOWEST index survives (highest index pruned first).
func TestPruneControllerKeepTopKTieBreak(t *testing.T) {
	pc := newPruneController(&ir.Prune{Score: "score", Keep: &ir.PruneKeep{K: 2}})
	if _, err := pc.Report(0, float64(0.5)); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.Report(1, float64(0.5)); err != nil {
		t.Fatal(err)
	}
	d, err := pc.Report(2, float64(0.5))
	if err != nil {
		t.Fatal(err)
	}
	if d.StopAll || !reflect.DeepEqual(d.PruneLosers, []int{2}) {
		t.Fatalf("tie-break: got %+v, want loser {2} (lowest index survives)", d)
	}
}

// TestPruneControllerStopWhen pins the stop_when policy: once the bounded bool
// expr over best.score is true, the decision is StopAll.
func TestPruneControllerStopWhen(t *testing.T) {
	pc := newPruneController(&ir.Prune{Score: "score", StopWhen: "{{ best.score >= 0.9 }}"})

	d, err := pc.Report(0, float64(0.1))
	if err != nil {
		t.Fatalf("Report(0): %v", err)
	}
	if d.StopAll {
		t.Fatalf("after 0.1: got StopAll, want not stopped")
	}

	d, err = pc.Report(1, float64(0.95))
	if err != nil {
		t.Fatalf("Report(1): %v", err)
	}
	if !d.StopAll {
		t.Fatalf("after 0.95: got %+v, want StopAll", d)
	}
}

// TestPruneControllerNonNumericScore: a non-numeric score is an error (the
// handler routes it to a permanent_failure, like a bad over).
func TestPruneControllerNonNumericScore(t *testing.T) {
	pc := newPruneController(&ir.Prune{Score: "score", Keep: &ir.PruneKeep{K: 1}})
	_, err := pc.Report(0, "not-a-number")
	if err == nil {
		t.Fatal("Report with a string score: want error, got nil")
	}
}

// TestBestScopeGrammar pins the stop_when scope grammar: best.score resolves to
// the running best; anything else is AWF4002.
func TestBestScopeGrammar(t *testing.T) {
	s := newBestScope(0.42)

	ref, err := template.ParseRef("best.score")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve(best.score): %v", err)
	}
	if v != float64(0.42) {
		t.Errorf("best.score = %v, want 0.42", v)
	}

	for _, bad := range []string{"best.foo", "step.x.y", "best"} {
		ref, err := template.ParseRef(bad)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", bad, err)
		}
		_, err = s.Resolve(ref)
		var ee *template.EvalError
		if err == nil || !errors.As(err, &ee) || ee.Code != template.EvalCodeRefUnresolved {
			t.Errorf("Resolve(%q): err = %v, want AWF4002 unresolved", bad, err)
		}
	}
}
