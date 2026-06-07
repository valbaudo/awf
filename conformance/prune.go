package conformance

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// SP5 Task 8 — prune: + reduce: COMPOSITION NOTE (do NOT implement here).
// `prune:` (this bucket) and `reduce:` (SP2) compose — the prune frontier's
// SURVIVORS are the input to a reduce: fold (search, then collapse). This bucket
// tests `prune:` STANDALONE; it has no dependency on reduce: code. The composed
// search-then-fold path is exercised once both clauses land on the same map; the
// man page (awf-workflow.5) documents the intended composition.

// pruneKeepWorkflow — SP5 keep: top(k). A map over 4 items; each body is one
// code step `hyp` declaring a numeric `score` output the frontier reads. keep:
// top(2) keeps the two highest scorers (item_passed) and prunes the other two
// (item_pruned) — a pruned item is NEITHER a pass NOR a failure, so the map ends
// ok even with min_success UNSET (= all NON-pruned passed). concurrency: 2 — the
// in-mem fake's shared Blobs serialize output_schema commits (the aggregation
// bucket's note), and the prune verdict is independent of dispatch order (the
// controller recomputes the full loser set from every reported score), so the
// survivor set {1,3} is deterministic regardless of concurrency.
var pruneKeepWorkflow = fmt.Sprintf(`workflow: conformance-prune-keep
version: 1
input:
  type: object
  required: [items]
  additionalProperties: false
  properties:
    items:
      type: array
      items: { type: string }
containers:
  c0:
    image: %s
graph:
  - map:
      over: "{{ input.items }}"
      as: x
      container: c0
      concurrency: 2
      body:
        - id: hyp
          container: c0
          run: "./hyp.sh {{ x }}"
          retry: { attempts: 1 }
          output_schema:
            type: object
            additionalProperties: false
            required: [score]
            properties:
              score: { type: number }
      prune:
        score: score
        keep: top(2)
`, fakeImageDigest)

// pruneStopWhenWorkflow — SP5 stop_when over the running best. concurrency: 1
// caps execution to ONE running body at a time. Every item scores >= 0.9, so the
// FIRST item to commit its score trips stop_when "best.score >= 0.9"; every
// still-queued item is then pruned BEFORE it acquires the single slot (its body
// never runs). WHICH index runs first is scheduler-dependent (goroutines launch
// in index order but the semaphore does not guarantee FIFO acquisition), so the
// test asserts the order-INDEPENDENT invariant stop_when guarantees: exactly ONE
// item passes, the rest are pruned, and only the trigger's body ran. Same body
// schema as pruneKeepWorkflow.
var pruneStopWhenWorkflow = fmt.Sprintf(`workflow: conformance-prune-stop-when
version: 1
input:
  type: object
  required: [items]
  additionalProperties: false
  properties:
    items:
      type: array
      items: { type: string }
containers:
  c0:
    image: %s
graph:
  - map:
      over: "{{ input.items }}"
      as: x
      container: c0
      concurrency: 1
      body:
        - id: hyp
          container: c0
          run: "./hyp.sh {{ x }}"
          retry: { attempts: 1 }
          output_schema:
            type: object
            additionalProperties: false
            required: [score]
            properties:
              score: { type: number }
      prune:
        score: score
        stop_when: "{{ best.score >= 0.9 }}"
`, fakeImageDigest)

// pruneTryWorkflow — SP5 a prune map nested in a try.do. A pruned item is NOT a
// failure, so the frontier discarding losers must NOT raise an error into the
// enclosing try → catch must NOT run. The catch step `sentinel` is deliberately
// NOT programmed: if a pruned item tripped catch, the fake's ProgramExec-miss
// would fire and the run would not be ok. Same keep: top(2) body as
// pruneKeepWorkflow.
var pruneTryWorkflow = fmt.Sprintf(`workflow: conformance-prune-try
version: 1
input:
  type: object
  required: [items]
  additionalProperties: false
  properties:
    items:
      type: array
      items: { type: string }
containers:
  c0:
    image: %s
graph:
  - try:
      do:
        - map:
            over: "{{ input.items }}"
            as: x
            container: c0
            concurrency: 2
            body:
              - id: hyp
                container: c0
                run: "./hyp.sh {{ x }}"
                retry: { attempts: 1 }
                output_schema:
                  type: object
                  additionalProperties: false
                  required: [score]
                  properties:
                    score: { type: number }
            prune:
              score: score
              keep: top(2)
      catch:
        - id: sentinel
          container: c0
          run: "./sentinel.sh"
          retry: { attempts: 1 }
`, fakeImageDigest)

// scoreProgram builds an execProgram returning a typed {score: v} for the prune
// body's `./hyp.sh <item>` command — the typed numeric output the frontier reads
// (typed outputs only; never parsed from text). Mirrors aggregation.go's mk().
func scoreProgram(item string, v float64) execProgram {
	raw, _ := json.Marshal(map[string]any{"score": v})
	return execProgram{
		cmd: "./hyp.sh " + item,
		res: container.ExecResult{ExitCode: 0, AWFOutput: raw},
	}
}

// testPrune is the SP5 conformance bucket — the prune: frontier on map against
// the fake backend (the "definition of done"). Sub-tests pin each load-bearing
// invariant from spec §3.2b + §7:
//
//   - prune_keep_top_k: 4 items, scores [0.1,0.9,0.5,0.7], keep: top(2); the two
//     highest (indices 1,3) commit item_passed, the two lowest (0,2) commit
//     item_pruned; the map ends ok.
//   - prune_not_counted_by_min_success: the same workflow with min_success UNSET
//     (= all) ends ok even though only 2 of 4 "passed" — pruned items are removed
//     from the denominator (the Task 4 fix), not counted as failures.
//   - prune_stop_when: concurrency 1, every item scores 0.95; the first to run
//     trips stop_when, the rest are pruned (bodies never run). The survivor index
//     is scheduler-dependent, so it asserts the order-independent invariant:
//     exactly 1 passes, the rest pruned, only the trigger's body ran; ok.
//   - prune_resume (THE resume-safety test): round 1 records {1,3}=passed,
//     {0,2}=pruned; round 2 resumes against a BARE fake (no programmed Exec). If
//     resume re-ran a pruned item's body the bare fake would error; if it
//     re-decided the prune the survivor set could change. Asserts the disposition
//     per index is byte-for-byte identical across resume and the map.item count
//     is unchanged — the journaled record is authoritative, never re-derived.
//   - prune_does_not_trip_try: a prune map in a try.do; pruned items are not
//     failures, so catch must not run.
func testPrune(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("prune_keep_top_k", func(t *testing.T) { testPruneKeepTopK(t, factory) })
	t.Run("prune_not_counted_by_min_success", func(t *testing.T) { testPruneNotCountedByMinSuccess(t, factory) })
	t.Run("prune_stop_when", func(t *testing.T) { testPruneStopWhen(t, factory) })
	t.Run("prune_resume", func(t *testing.T) { testPruneResume(t, factory) })
	t.Run("prune_does_not_trip_try", func(t *testing.T) { testPruneDoesNotTripTry(t, factory) })
}

// keepTopKPrograms is the score schedule shared by the keep-top-k, min_success,
// and resume sub-tests: scores [0.1,0.9,0.5,0.7] over items a,b,c,d. The top-2
// (b=0.9 → index 1, d=0.7 → index 3) survive; a=0.1 (index 0) and c=0.5 (index 2)
// are pruned. The rank is independent of dispatch order, so the survivor set is
// deterministic at any concurrency.
func keepTopKPrograms() []execProgram {
	return []execProgram{
		scoreProgram("a", 0.1),
		scoreProgram("b", 0.9),
		scoreProgram("c", 0.5),
		scoreProgram("d", 0.7),
	}
}

// itemStatusByN folds the log and returns a map of (mapPath item) N → committed
// map.item Status. mapItems is the total number of map.item events seen.
func itemStatusByN(events []state.Event) (byN map[int]string, mapItems int) {
	byN = map[int]string{}
	for _, e := range events {
		if e.Type != engine.EventMapItem {
			continue
		}
		mapItems++
		var d engine.MapItemData
		_ = json.Unmarshal(e.Data, &d)
		byN[d.N] = d.Status
	}
	return byN, mapItems
}

func testPruneKeepTopK(t *testing.T, factory BackendFactory) {
	t.Helper()
	programmed := preProgramFake(t, factory, keepTopKPrograms())
	h := newHarnessWithInput(t, programmed, pruneKeepWorkflow,
		map[string]any{"items": []any{"a", "b", "c", "d"}})

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("prune_keep_top_k: runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("prune_keep_top_k: outcome = %q, want ok", oc)
	}

	byN, total := itemStatusByN(mustFoldEvents(t, h))
	if total != 4 {
		t.Fatalf("prune_keep_top_k: map.item count = %d, want 4 (every item commits a durable terminal status)", total)
	}
	want := map[int]string{
		0: engine.ItemPruned, // a=0.1 — lowest, pruned
		1: engine.ItemPassed, // b=0.9 — top-2 survivor
		2: engine.ItemPruned, // c=0.5 — pruned
		3: engine.ItemPassed, // d=0.7 — top-2 survivor
	}
	for n, w := range want {
		if byN[n] != w {
			t.Errorf("prune_keep_top_k: item N=%d status=%q, want %q", n, byN[n], w)
		}
	}

	// A pruned item is not a failure — no node.failed in a pruned map.
	for _, e := range mustFoldEvents(t, h) {
		if e.Type == engine.EventNodeFailed {
			t.Errorf("prune_keep_top_k: unexpected node.failed in a pruned map: %+v", e)
		}
	}
}

func testPruneNotCountedByMinSuccess(t *testing.T, factory BackendFactory) {
	t.Helper()
	// pruneKeepWorkflow leaves min_success UNSET (= all). With the Task 4 fix,
	// pruned items are removed from BOTH the numerator and the denominator, so the
	// map succeeds because every NON-pruned item (2 of 2 survivors) passed — even
	// though only 2 of the 4 baseline items "passed". Without the denominator fix
	// the map would require all 4 to pass and return retryable_failure.
	programmed := preProgramFake(t, factory, keepTopKPrograms())
	h := newHarnessWithInput(t, programmed, pruneKeepWorkflow,
		map[string]any{"items": []any{"a", "b", "c", "d"}})

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("prune_not_counted_by_min_success: runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("prune_not_counted_by_min_success: outcome = %q, want ok (pruned items are removed from the min_success denominator)", oc)
	}

	byN, total := itemStatusByN(mustFoldEvents(t, h))
	if total != 4 {
		t.Fatalf("prune_not_counted_by_min_success: map.item count = %d, want 4", total)
	}
	var pass, pruned int
	for _, s := range byN {
		switch s {
		case engine.ItemPassed:
			pass++
		case engine.ItemPruned:
			pruned++
		default:
			t.Errorf("prune_not_counted_by_min_success: unexpected status %q", s)
		}
	}
	if pass != 2 || pruned != 2 {
		t.Errorf("prune_not_counted_by_min_success: pass=%d pruned=%d, want pass=2 pruned=2", pass, pruned)
	}
}

func testPruneStopWhen(t *testing.T, factory BackendFactory) {
	t.Helper()
	// Every item scores 0.95, so the FIRST item to commit trips
	// stop_when "best.score >= 0.9"; concurrency 1 means every other item is then
	// pruned (queued-loser short-circuit) before it acquires the single slot — its
	// body never runs. WHICH index runs first is scheduler-dependent, so this
	// asserts the order-independent invariant: exactly 1 passes, the rest are
	// pruned, and only the trigger's body executed. The map ends ok.
	var runFake *container.Fake
	capturing := func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		for _, p := range []execProgram{
			scoreProgram("a", 0.95),
			scoreProgram("b", 0.95),
			scoreProgram("c", 0.95),
		} {
			fake.ProgramExec(p.cmd, p.res, nil)
		}
		runFake = fake
		return fake
	}
	h := newHarnessWithInput(t, capturing, pruneStopWhenWorkflow,
		map[string]any{"items": []any{"a", "b", "c"}})

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("prune_stop_when: runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("prune_stop_when: outcome = %q, want ok", oc)
	}

	byN, total := itemStatusByN(mustFoldEvents(t, h))
	if total != 3 {
		t.Fatalf("prune_stop_when: map.item count = %d, want 3", total)
	}
	var pass, pruned int
	for n, s := range byN {
		switch s {
		case engine.ItemPassed:
			pass++
		case engine.ItemPruned:
			pruned++
		default:
			t.Errorf("prune_stop_when: item N=%d unexpected status %q", n, s)
		}
	}
	if pass != 1 || pruned != 2 {
		t.Errorf("prune_stop_when: pass=%d pruned=%d, want pass=1 pruned=2 (the first committer trips stop_when; the rest are pruned)", pass, pruned)
	}

	// Exactly one body ran: the pruned items were short-circuited before dispatch
	// (their bodies never executed). Count ./hyp.sh exec calls on the fake.
	if runFake == nil {
		t.Skip("prune_stop_when: backend is not *container.Fake; body-count proof is fake-only")
	}
	bodyCalls := 0
	for _, c := range runFake.Calls {
		if c.Run == "./hyp.sh a" || c.Run == "./hyp.sh b" || c.Run == "./hyp.sh c" {
			bodyCalls++
		}
	}
	if bodyCalls != 1 {
		t.Errorf("prune_stop_when: %d body executions, want exactly 1 (pruned items short-circuit before dispatch); calls = %+v", bodyCalls, runFake.Calls)
	}
}

func testPruneResume(t *testing.T, factory BackendFactory) {
	t.Helper()
	// THE resume-safety test (SP5's load-bearing correctness property): a prune
	// decision is DURABLE and REPLAYED, never re-decided. Round 1 runs to
	// completion and records the per-index disposition. Round 2 resumes against a
	// BARE fake (no programmed Exec): if resume re-ran a pruned item's body the
	// bare fake would error ("no programmed result"); if resume RE-DECIDED the
	// prune (instead of replaying the journal) the survivor set could change. The
	// assertions below catch both — the committed map.item{item_pruned} events are
	// authoritative.

	// Round 1: program all 4 scores; keep: top(2). Run completes ok with 4
	// map.item events ({1,3}=passed, {0,2}=pruned).
	factory1 := preProgramFake(t, factory, keepTopKPrograms())
	h := newHarnessWithInput(t, factory1, pruneKeepWorkflow,
		map[string]any{"items": []any{"a", "b", "c", "d"}})
	oc1, err1 := h.runWorkflow(t)
	if err1 != nil {
		t.Fatalf("prune_resume: round-1 err = %v", err1)
	}
	if oc1 != engine.OutcomeOK {
		t.Fatalf("prune_resume: round-1 outcome = %q, want ok", oc1)
	}

	round1ByN, round1Total := itemStatusByN(mustFoldEvents(t, h))
	if round1Total != 4 {
		t.Fatalf("prune_resume: round-1 map.item count = %d, want 4", round1Total)
	}
	round1Want := map[int]string{
		0: engine.ItemPruned,
		1: engine.ItemPassed,
		2: engine.ItemPruned,
		3: engine.ItemPassed,
	}
	for n, w := range round1Want {
		if round1ByN[n] != w {
			t.Fatalf("prune_resume: round-1 item N=%d status=%q, want %q", n, round1ByN[n], w)
		}
	}

	// Round 2: BARE fake (no programmed entries). Committed items — survivors AND
	// pruned losers — replay from the journal; nothing re-executes. If a pruned
	// item's body re-ran, the bare fake would error; if the prune were re-decided,
	// the survivor set could differ.
	h.factory = factory
	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Fatalf("prune_resume: round-2 err = %v (resume must replay committed survivors AND pruned items, never re-run a pruned body or re-decide the prune)", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Errorf("prune_resume: round-2 outcome = %q, want ok", oc2)
	}

	// The disposition per index is BYTE-FOR-BYTE the same as round 1, and the
	// total map.item count is still 4 (no new commits, no duplicates per N) — the
	// journaled record is authoritative; the frontier is NOT re-derived.
	round2ByN, round2Total := itemStatusByN(mustFoldEvents(t, h))
	if round2Total != 4 {
		t.Errorf("prune_resume: post-resume map.item total = %d, want 4 (resume must not re-commit; the durable prune record is replayed)", round2Total)
	}
	for n, w := range round1Want {
		if round2ByN[n] != w {
			t.Errorf("prune_resume: post-resume item N=%d status=%q, want %q (the prune decision is replayed verbatim, never re-decided)", n, round2ByN[n], w)
		}
	}

	// Per-N commit-once: each item index appears in exactly one map.item event
	// across the resumed log (replay must not duplicate a pruned/passed commit).
	perN := map[int]int{}
	for _, e := range mustFoldEvents(t, h) {
		if e.Type != engine.EventMapItem {
			continue
		}
		var d engine.MapItemData
		if uErr := json.Unmarshal(e.Data, &d); uErr != nil {
			t.Fatalf("prune_resume: unmarshal map.item: %v", uErr)
		}
		perN[d.N]++
	}
	for n, count := range perN {
		if count != 1 {
			t.Errorf("prune_resume: item N=%d committed %d times; want 1 (replay must not duplicate)", n, count)
		}
	}
}

func testPruneDoesNotTripTry(t *testing.T, factory BackendFactory) {
	t.Helper()
	// A pruned item is NOT a failure, so a prune map inside a try.do must NOT
	// cause the try to enter catch. The catch step `sentinel` is deliberately NOT
	// programmed: if a pruned item raised an error into the try, catch would run
	// the (unprogrammed) ./sentinel.sh and the run would not complete ok. Pins the
	// load-bearing "pruned is not a failure" invariant at the control-flow level.
	programmed := preProgramFake(t, factory, keepTopKPrograms())
	h := newHarnessWithInput(t, programmed, pruneTryWorkflow,
		map[string]any{"items": []any{"a", "b", "c", "d"}})

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("prune_does_not_trip_try: runWorkflow: %v (pruned items must not raise an error into the enclosing try)", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("prune_does_not_trip_try: outcome = %q, want ok (pruned items do not trip try → catch)", oc)
	}

	// catch's sentinel must NOT have committed — the prune frontier discarding
	// losers raised no error, so the try never entered catch.
	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("prune_does_not_trip_try: Fold: %v", ferr)
	}
	if _, ok := rs.LookupCompleted("try[0].catch.sentinel"); ok {
		t.Errorf("prune_does_not_trip_try: catch sentinel committed; a pruned map must not trip try → catch")
	}

	// The inner map's items committed as expected (2 passed, 2 pruned).
	byN, total := itemStatusByN(mustFoldEvents(t, h))
	if total != 4 {
		t.Fatalf("prune_does_not_trip_try: map.item count = %d, want 4", total)
	}
	var pass, pruned int
	for _, s := range byN {
		switch s {
		case engine.ItemPassed:
			pass++
		case engine.ItemPruned:
			pruned++
		}
	}
	if pass != 2 || pruned != 2 {
		t.Errorf("prune_does_not_trip_try: pass=%d pruned=%d, want pass=2 pruned=2", pass, pruned)
	}
}
