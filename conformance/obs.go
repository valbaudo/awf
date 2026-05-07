package conformance

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/obs"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// testObs is Bucket 16 (Phase 6 slice 6.3). It locks obs.Project as a
// deterministic read-only projection of a fake-backend run's log, over
// obs-OWNED self-contained fixtures (no cross-bucket dependency — decision 3).
// No Docker, no LLM, no engine changes. See the design spec
// 2026-05-02-awf-phase6-design.md decision 10.
func testObs(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("span_tree_mirrors_addressing", func(t *testing.T) { testObsSpanTreeMirrorsAddressing(t, factory) })
	t.Run("byte_identical_replay", func(t *testing.T) { testObsByteIdenticalReplay(t, factory) })
	t.Run("truncated_log_pending", func(t *testing.T) { testObsTruncatedLogPending(t, factory) })
	t.Run("local_exporter_roundtrips", func(t *testing.T) { testObsLocalExporterRoundTrips(t, factory) })
	t.Run("gate_evaluation_result", func(t *testing.T) { testObsGateEvaluationResult(t, factory) })
	t.Run("cost_rollup_scope_not_summed", func(t *testing.T) { testObsCostRollupScopeNotSummed(t, factory) })
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

// testObsByteIdenticalReplay — Regression guarded: introducing wall-clock or
// map-iteration-order nondeterminism into Project (so two folds of the same log
// diverge) is caught here at the full engine->log->project path. (Unit-level
// idempotence is already covered by obs/determinism_test.go; the conformance
// value is the real-engine-produced log.) A JSON-byte assertion was rejected as
// redundant — DeepEqual is the stricter relation.
func testObsByteIdenticalReplay(t *testing.T, factory BackendFactory) {
	t.Helper()
	h := runObsScopeFixture(t, factory)
	spans1, err := obs.Project(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("obs.Project (first): %v", err)
	}
	spans2, err := obs.Project(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("obs.Project (replay): %v", err)
	}
	if !reflect.DeepEqual(spans1, spans2) {
		t.Fatalf("projection not deterministic on replay")
	}
}

// testObsTruncatedLogPending — Regression guarded: failing to mark an
// unfinalized node.started (no terminal node.completed/failed) as Pending. OTel
// has no notion of a still-open span at export time; obs derives the
// in-flight/crashed state from the log and marks the span Pending (spec App. A).
func testObsTruncatedLogPending(t *testing.T, factory BackendFactory) {
	t.Helper()
	h := runObsScopeFixture(t, factory)
	events := mustFoldEvents(t, h)
	cut := -1
	for i, e := range events {
		if e.Type == engine.EventNodeStarted {
			cut = i // first node.started is a STEP node (scopes never get node.started)
			break
		}
	}
	if cut < 0 {
		t.Fatalf("no node.started event in the log (%d events)", len(events))
	}
	startedPath := events[cut].Path
	spans, err := obs.Project(events[:cut+1], h.blobs) // run.started ... first node.started
	if err != nil {
		t.Fatalf("obs.Project (truncated): %v", err)
	}
	s, ok := findObsSpan(spans, startedPath)
	if !ok {
		t.Fatalf("no span for unfinalized path %q", startedPath)
	}
	if !s.Pending {
		t.Errorf("span %q: Pending = false, want true (started, never finalized)", startedPath)
	}
}

// obsVerdictRejectedJSON is obs.go's OWN copy of a rejecting verdict — the same
// shape as gate.go's verdictRejectedJSON, copied so the obs bucket does not
// depend on Bucket 5's const (decision 3).
const obsVerdictRejectedJSON = `{"verified":false,"feedback":"nope"}`

// obsGateRejectedWorkflow — obs-owned, self-contained 1-step-per-block gate whose
// evaluator always returns obsVerdictRejectedJSON, so it exhausts max_attempts:2
// and rejects. Emits gate.attempt events + a rich gate scope tree. Used by (d)/(e).
var obsGateRejectedWorkflow = fmt.Sprintf(`workflow: conformance-obs-gate-rejected
version: 1
containers:
  lab:
    image: %s
graph:
  - gate:
      generate:
        - id: gen
          container: lab
          run: "./gen.sh"
          retry: { attempts: 1 }
      evaluate:
        - id: eval
          container: lab
          run: "./eval.sh"
          retry: { attempts: 1 }
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, feedback]
            properties:
              verified: { type: boolean }
              feedback: { type: string }
      until: "{{ evaluate.verified }}"
      max_attempts: 2
`, fakeImageDigest)

// runObsGateFixture runs obsGateRejectedWorkflow on the fake backend (rejects
// after 2 attempts) and returns the harness. Used by (d)/(e). The generator
// command is static "./gen.sh" (no feedback template), so one ProgramExec entry
// serves both attempts (the fake keys by command).
func runObsGateFixture(t *testing.T, factory BackendFactory) *harness {
	t.Helper()
	pf := preProgramFake(t, factory, []execProgram{
		{cmd: "./gen.sh", res: container.ExecResult{ExitCode: 0}},
		{cmd: "./eval.sh", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(obsVerdictRejectedJSON)}},
	})
	h := newHarness(t, pf, obsGateRejectedWorkflow)
	oc, err := h.runWorkflow(t)
	if oc != engine.OutcomeRejected {
		t.Fatalf("obs gate fixture: outcome = %q, want %q", oc, engine.OutcomeRejected)
	}
	if err == nil {
		t.Fatalf("obs gate fixture: err = nil, want non-nil (a rejected gate propagates)")
	}
	return h
}

// testObsLocalExporterRoundTrips — Regression guarded: a dropping exporter
// (span count drifts) or a mangling exporter (attribute VALUE corrupted). The
// gate scope is found by its awf.node.path ATTRIBUTE, not SpanStub.Name — the
// OTel span Name is the low-cardinality scope kind "gate".
func testObsLocalExporterRoundTrips(t *testing.T, factory BackendFactory) {
	t.Helper()
	h := runObsGateFixture(t, factory)
	spans, err := obs.Project(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("obs.Project: %v", err)
	}

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	ctx := context.Background()
	if err := obs.Export(ctx, spans, tp); err != nil {
		t.Fatalf("obs.Export: %v", err)
	}
	got := exp.GetSpans() // read BEFORE Shutdown clears the store
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("TracerProvider.Shutdown: %v", err)
	}

	if len(got) != len(spans) {
		t.Fatalf("exporter received %d spans, want %d (one OTel span per obs.Span)", len(got), len(spans))
	}

	attrVal := func(kvs []attribute.KeyValue, key string) (attribute.Value, bool) {
		for _, kv := range kvs {
			if string(kv.Key) == key {
				return kv.Value, true
			}
		}
		return attribute.Value{}, false
	}
	found := false
	for i := range got {
		v, ok := attrVal(got[i].Attributes, obs.AttrNodePath)
		if !ok || v.AsString() != "gate[0]" {
			continue
		}
		found = true
		outcome, ok := attrVal(got[i].Attributes, obs.AttrGateOutcome)
		if !ok {
			t.Fatalf("gate[0] stub missing %s; attrs = %v", obs.AttrGateOutcome, got[i].Attributes)
		}
		if outcome.AsString() != "rejected" {
			t.Errorf("%s = %q, want %q", obs.AttrGateOutcome, outcome.AsString(), "rejected")
		}
		break
	}
	if !found {
		t.Fatalf("no exported span with %s == %q", obs.AttrNodePath, "gate[0]")
	}
}

// obsCostParallelWorkflow — a parallel of two distinct-container agent steps
// (the validator requires distinct containers for parallel step branches). Both
// use ref "test/worker"; the bucket scripts BOTH invocation indices with the
// same Cost (0.01), so the rollup (0.01+0.01=0.02) is independent of the order
// the concurrent Launches consume indices. No output_schema (outputs unreferenced).
var obsCostParallelWorkflow = fmt.Sprintf(`workflow: conformance-obs-cost-parallel
version: 1
containers:
  c0:
    image: %[1]s
  c1:
    image: %[1]s
graph:
  - parallel:
      - id: worker0
        container: c0
        uses: test/worker
        with:
          prompt: "work"
      - id: worker1
        container: c1
        uses: test/worker
        with:
          prompt: "work"
`, fakeImageDigest)

// testObsCostRollupScopeNotSummed — Regression guarded: summing scope spans into
// the rollup (e.g. removing the s.Scope skip in sumLeafCostsUSD) -> root rollup
// becomes 0.04 not 0.02. This is the ONLY double-count axis obs can see: the
// run rollup sums LEAF step costs only, so the synthesized parallel scope never
// double-counts its children. (The separate ADAPTER message-ID dedup is upstream
// — obs receives one MetricCost per step and never sees message IDs.)
func testObsCostRollupScopeNotSummed(t *testing.T, factory BackendFactory) {
	t.Helper()
	register := func(reg *agent.Registry) {
		fk := fake.New("test/worker").
			Script(0, fake.Result{Cost: 0.01}).
			Script(1, fake.Result{Cost: 0.01})
		if err := reg.Register(fk); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	h := newHarnessWithAgentRegistry(t, factory, obsCostParallelWorkflow, register)
	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want %q", oc, engine.OutcomeOK)
	}

	spans, err := obs.Project(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("obs.Project: %v", err)
	}

	var leafCount int
	var leafSum float64
	for _, s := range spans {
		c, has := s.Attributes[obs.AttrCostUSD].(float64)
		if !has {
			continue
		}
		if s.Scope {
			t.Errorf("scope span %q carries %s = %v (double-count hazard)", s.Path, obs.AttrCostUSD, c)
			continue
		}
		leafCount++
		leafSum += c
		if c < 0.0099 || c > 0.0101 {
			t.Errorf("leaf span %q cost = %v, want ~0.01", s.Path, c)
		}
	}
	if leafCount != 2 {
		t.Errorf("got %d leaf spans with a cost, want 2", leafCount)
	}

	root, ok := findObsSpan(spans, "")
	if !ok {
		t.Fatal("no root span")
	}
	rollup, ok := root.Attributes[obs.AttrRunCostUSD].(float64)
	if !ok {
		t.Fatalf("root span missing %s (float64); attrs = %v", obs.AttrRunCostUSD, root.Attributes)
	}
	if rollup < 0.0199 || rollup > 0.0201 {
		t.Errorf("%s = %v, want ~0.02 (NOT double-counted to 0.04)", obs.AttrRunCostUSD, rollup)
	}
	if rollup < leafSum-1e-9 || rollup > leafSum+1e-9 {
		t.Errorf("root rollup %v != sum of leaf costs %v", rollup, leafSum)
	}
}

// testObsGateEvaluationResult — Regression guarded: miswiring gate.attempt ->
// gen_ai.evaluation.result (wrong event count, name, or outcome/attempts attr).
// The obs-owned gate rejects on both attempts -> 2 evaluation events, outcome
// "rejected", attempts 2.
func testObsGateEvaluationResult(t *testing.T, factory BackendFactory) {
	t.Helper()
	h := runObsGateFixture(t, factory)
	spans, err := obs.Project(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("obs.Project: %v", err)
	}
	gate, ok := findObsSpan(spans, "gate[0]")
	if !ok {
		t.Fatalf("no gate span at %q", "gate[0]")
	}
	if got := gate.Attributes[obs.AttrGateAttempts]; got != int64(2) {
		t.Errorf("%s = %v (%T), want int64(2)", obs.AttrGateAttempts, got, got)
	}
	if got := gate.Attributes[obs.AttrGateOutcome]; got != "rejected" {
		t.Errorf("%s = %v, want %q", obs.AttrGateOutcome, got, "rejected")
	}
	if len(gate.Events) != 2 {
		t.Fatalf("gate span has %d events, want 2 (one gen_ai.evaluation.result per attempt)", len(gate.Events))
	}
	for i, e := range gate.Events {
		if e.Name != obs.EventGenAIEvaluation {
			t.Errorf("gate.Events[%d].Name = %q, want %q", i, e.Name, obs.EventGenAIEvaluation)
		}
	}
}
