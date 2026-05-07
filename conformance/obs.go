package conformance

import (
	"fmt"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/obs"
)

// testObs is Bucket 16 (Phase 6 slice 6.3). It locks obs.Project as a
// deterministic read-only projection of a fake-backend run's log, over
// obs-OWNED self-contained fixtures (no cross-bucket dependency — decision 3).
// No Docker, no LLM, no engine changes. See the design spec
// 2026-05-02-awf-phase6-design.md decision 10.
func testObs(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("span_tree_mirrors_addressing", func(t *testing.T) { testObsSpanTreeMirrorsAddressing(t, factory) })
}

// obsScopeTreeWorkflow — obs-owned, self-contained (decision 3). An all-ok
// loop with a 2-step body yields a multi-level addressing tree
// (loop[0] / loop[0].iter-{0,1} scopes + leaf steps) with no gate, no input,
// no agent cost. Used by sub-tests (a)/(b)/(c). Does NOT borrow Bucket 5's
// gateFeedbackThreadingWorkflow.
var obsScopeTreeWorkflow = fmt.Sprintf(`workflow: conformance-obs-scope-tree
version: 1
containers:
  lab:
    image: %s
graph:
  - loop:
      max_iters: 2
      body:
        - id: prep
          container: lab
          run: "./prep.sh"
          retry: { attempts: 1 }
        - id: work
          container: lab
          run: "./work.sh"
          retry: { attempts: 1 }
`, fakeImageDigest)

// findObsSpan returns the span at the given addressing path. (obs's own findSpan
// is a _test.go symbol, not visible here.)
//
//nolint:unused // reserved for sub-tests (b)/(c) added in later slices
func findObsSpan(spans []obs.Span, path string) (obs.Span, bool) {
	for _, s := range spans {
		if s.Path == path {
			return s, true
		}
	}
	return obs.Span{}, false
}

// runObsScopeFixture runs obsScopeTreeWorkflow on the fake backend (all-ok) and
// returns the harness so callers can fold the log. Used by (a)/(b)/(c).
func runObsScopeFixture(t *testing.T, factory BackendFactory) *harness {
	t.Helper()
	pf := preProgramFake(t, factory, []execProgram{
		{cmd: "./prep.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./work.sh", res: container.ExecResult{ExitCode: 0}},
	})
	h := newHarness(t, pf, obsScopeTreeWorkflow)
	oc, err := h.runWorkflow(t)
	if err != nil || oc != engine.OutcomeOK {
		t.Fatalf("obs scope fixture: (outcome, err) = (%q, %v), want (ok, nil)", oc, err)
	}
	return h
}

// testObsSpanTreeMirrorsAddressing — Regression guarded: dropping a synthesized
// scope (synthesizeScopes) -> the ancestor-walk finds a gap; inventing a spurious
// span -> the no-spurious check flags it; mislabeling a leaf as a scope (or vice
// versa) -> the Scope check flags it.
//
// The leaf-event set INCLUDES node.skipped (decision 6): skip leaves can land at
// scope-shaped paths, and omitting node.skipped would false-positive the
// no-spurious + Scope checks on any correct projection containing a skip.
func testObsSpanTreeMirrorsAddressing(t *testing.T, factory BackendFactory) {
	t.Helper()
	h := runObsScopeFixture(t, factory)
	events := mustFoldEvents(t, h)
	spans, err := obs.Project(events, h.blobs)
	if err != nil {
		t.Fatalf("obs.Project: %v", err)
	}

	byPath := map[string]obs.Span{}
	for _, s := range spans {
		byPath[s.Path] = s
	}
	if _, ok := byPath[""]; !ok {
		t.Fatalf("no root span (Path == %q)", "")
	}

	leafPaths := map[string]bool{}
	for _, e := range events {
		switch e.Type {
		case engine.EventNodeStarted, engine.EventNodeCompleted,
			engine.EventNodeFailed, engine.EventNodeSkipped:
			if e.Path != "" {
				leafPaths[e.Path] = true
			}
		}
	}

	isAncestorOfLeaf := func(p string) bool {
		for lp := range leafPaths {
			for cur := lp; ; {
				parent, ok := engine.ParentPath(cur)
				if !ok {
					break
				}
				if parent == p {
					return true
				}
				cur = parent
			}
		}
		return false
	}

	for _, s := range spans {
		if s.Path == "" {
			continue // root: special-cased (Scope=true, path "")
		}
		if !leafPaths[s.Path] && !isAncestorOfLeaf(s.Path) {
			t.Errorf("spurious span %q (scope=%v kind=%q): not a leaf path nor an ancestor of one", s.Path, s.Scope, s.Kind)
		}
		if leafPaths[s.Path] {
			if s.Scope {
				t.Errorf("leaf span %q has Scope=true", s.Path)
			}
		} else if !s.Scope {
			t.Errorf("synthesized scope span %q has Scope=false", s.Path)
		}
		for cur := s.Path; ; {
			parent, ok := engine.ParentPath(cur)
			if !ok {
				break
			}
			if _, exists := byPath[parent]; !exists {
				t.Errorf("span %q ancestor %q has no span (tree not connected)", s.Path, parent)
			}
			cur = parent
		}
	}
	for lp := range leafPaths {
		if _, ok := byPath[lp]; !ok {
			t.Errorf("leaf-event path %q has no span", lp)
		}
	}
}
