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

// pruneKeepWorkflow — SP5 keep: <k>. A map over 4 items; each body is one
// code step `hyp` declaring a numeric `score` output the frontier reads. keep:
// 2 keeps the two highest scorers (item_passed) and prunes the other two
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
        keep: 2
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

// prunePartialCrashWorkflow — keep: 1 at concurrency 1, used by the
// partial-crash resume test. concurrency 1 runs the four bodies STRICTLY
// sequentially, so the body-append count is deterministic and the crash can be
// aimed precisely at the single map.frontier append. Scores [0.9,0.1,0.2,0.3]
// make item 0 the SOLE top-1 survivor; under a (buggy) per-item commit a crash
// after item 0 committed would re-derive the remainder against an empty
// controller and keep a SECOND survivor — the >k corruption this test guards.
var prunePartialCrashWorkflow = fmt.Sprintf(`workflow: conformance-prune-partial-crash
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
        keep: 1
`, fakeImageDigest)

// pruneTryWorkflow — SP5 a prune map nested in a try.do. A pruned item is NOT a
// failure, so the frontier discarding losers must NOT raise an error into the
// enclosing try → catch must NOT run. The catch step `sentinel` is deliberately
// NOT programmed: if a pruned item tripped catch, the fake's ProgramExec-miss
// would fire and the run would not be ok. Same keep: 2 body as
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
              keep: 2
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

// scoreAgreeProgram is scoreProgram plus a boolean `agree` the quorum reducer
// counts — for the prune: + reduce:{quorum} composition test.
func scoreAgreeProgram(item string, v float64, agree bool) execProgram {
	raw, _ := json.Marshal(map[string]any{"score": v, "agree": agree})
	return execProgram{
		cmd: "./hyp.sh " + item,
		res: container.ExecResult{ExitCode: 0, AWFOutput: raw},
	}
}

// pruneQuorumWorkflow — the prune: + reduce:{quorum} composition. keep: 2
// over 4 items, then a quorum: 1.0 (unanimous) reduce over the SURVIVORS' `agree`
// field. The quorum cohort must be the NON-PRUNED count (2 survivors), not the
// full fan-out (4): with cohort=2 the two unanimous survivors meet quorum (need
// 2, agree 2 → passed, votes 2). The old behavior measured quorum against
// len(over)=4, so unanimous survivors could never reach need=4 — a wrong
// retryable_failure on a documented feature combination.
var pruneQuorumWorkflow = fmt.Sprintf(`workflow: conformance-prune-quorum
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
            required: [score, agree]
            properties:
              score: { type: number }
              agree: { type: boolean }
      prune:
        score: score
        keep: 2
      reduce:
        quorum: 1.0
        over: agree
`, fakeImageDigest)

// testPrune is the SP5 conformance bucket — the prune: frontier on map against
// the fake backend (the "definition of done"). Sub-tests pin each load-bearing
// invariant from spec §3.2b + §7:
//
//   - prune_keep_top_k: 4 items, scores [0.1,0.9,0.5,0.7], keep: 2; the two
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
	t.Run("prune_partial_crash_frontier", func(t *testing.T) { testPrunePartialCrashFrontier(t, factory) })
	t.Run("prune_quorum_cohort_excludes_pruned", func(t *testing.T) { testPruneQuorumCohort(t, factory) })
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
// disposition Status, plus dispositions, the total number of per-item
// dispositions seen. A PLAIN map records one map.item event per item; a PRUNE
// map records its whole disposition in ONE atomic map.frontier event (SP5) — this
// counts both so prune-map assertions (e.g. "4 items committed") read the same
// whether they land in 4 map.item events or 1 map.frontier of 4 items.
func itemStatusByN(events []state.Event) (byN map[int]string, dispositions int) {
	byN = map[int]string{}
	for _, e := range events {
		switch e.Type {
		case engine.EventMapItem:
			var d engine.MapItemData
			_ = json.Unmarshal(e.Data, &d)
			byN[d.N] = d.Status
			dispositions++
		case engine.EventMapFrontier:
			var d engine.MapFrontierData
			_ = json.Unmarshal(e.Data, &d)
			for _, it := range d.Items {
				byN[it.N] = it.Status
				dispositions++
			}
		}
	}
	return byN, dispositions
}

// dispositionCountByN tallies how many times each item index commits a
// disposition across the log (map.item OR a map.frontier element). Used to assert
// commit-once: replay must never re-emit a frontier or duplicate a per-item
// commit.
func dispositionCountByN(events []state.Event) map[int]int {
	perN := map[int]int{}
	for _, e := range events {
		switch e.Type {
		case engine.EventMapItem:
			var d engine.MapItemData
			_ = json.Unmarshal(e.Data, &d)
			perN[d.N]++
		case engine.EventMapFrontier:
			var d engine.MapFrontierData
			_ = json.Unmarshal(e.Data, &d)
			for _, it := range d.Items {
				perN[it.N]++
			}
		}
	}
	return perN
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

	// Round 1: program all 4 scores; keep: 2. Run completes ok with 4
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

	// Per-N commit-once: each item index commits exactly one disposition across
	// the resumed log (replay must not re-emit the frontier or duplicate a commit).
	for n, count := range dispositionCountByN(mustFoldEvents(t, h)) {
		if count != 1 {
			t.Errorf("prune_resume: item N=%d committed %d times; want 1 (replay must not duplicate)", n, count)
		}
	}
}

func testPrunePartialCrashFrontier(t *testing.T, factory BackendFactory) {
	t.Helper()
	// THE atomic-frontier resume-safety test. A prune disposition is a GLOBAL
	// top(k) decision committed as ONE atomic map.frontier event. Round 1 crashes
	// AT that single append: every body has committed its node.completed, but the
	// frontier has not. Under a (buggy) per-item commit a crash here would leave a
	// PARTIAL frontier — resume would skip the committed survivor (its score never
	// re-fed) and re-derive the rest against an EMPTY controller, keeping MORE
	// survivors than keep: 1 allows. The atomic event makes that impossible: a
	// crash before it leaves ZERO committed dispositions, so resume re-derives the
	// WHOLE frontier over the full (replayed) score set — exactly one survivor.
	scores := []execProgram{
		scoreProgram("a", 0.9), // index 0 — the sole top-1 survivor
		scoreProgram("b", 0.1),
		scoreProgram("c", 0.2),
		scoreProgram("d", 0.3),
	}
	h := newHarnessWithInput(t, preProgramFake(t, factory, scores), prunePartialCrashWorkflow,
		map[string]any{"items": []any{"a", "b", "c", "d"}})

	// Crash at the map.frontier append. With concurrency 1 the bodies run strictly
	// sequentially, so the appends before the frontier are deterministic:
	// run.started(1) + 4×(node.started + node.completed)(8) = 9; the frontier is the
	// 10th append, which FailAppendAfterN(9) fails.
	h.log.FailAppendAfterN(9)
	if _, err := h.runWorkflow(t); err == nil {
		t.Fatal("prune_partial_crash: round-1 err = nil, want induced-fault error at the map.frontier append")
	}

	// Atomicity: the frontier did NOT commit, so ZERO dispositions are durable —
	// never a partial subset (a per-item commit would have left 1+ survivors here).
	if _, round1 := itemStatusByN(mustFoldEvents(t, h)); round1 != 0 {
		t.Fatalf("prune_partial_crash: round-1 committed %d dispositions, want 0 (the frontier commit is atomic — a crash leaves no partial disposition)", round1)
	}

	// Resume against a BARE fake: every body committed its node.completed before the
	// crash, so all four REPLAY (no programmed Exec needed) — only the cheap frontier
	// re-derives, from the replayed scores, and must keep exactly the global top-1.
	h.log.ClearFault()
	h.factory = factory
	oc, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("prune_partial_crash: resume err = %v (bodies must replay; only the frontier re-derives)", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("prune_partial_crash: resume outcome = %q, want ok", oc)
	}

	byN, total := itemStatusByN(mustFoldEvents(t, h))
	if total != 4 {
		t.Fatalf("prune_partial_crash: post-resume disposition count = %d, want 4", total)
	}
	want := map[int]string{
		0: engine.ItemPassed, // a=0.9 — the sole top-1 survivor
		1: engine.ItemPruned, // b=0.1
		2: engine.ItemPruned, // c=0.2
		3: engine.ItemPruned, // d=0.3
	}
	survivors := 0
	for n, w := range want {
		if byN[n] != w {
			t.Errorf("prune_partial_crash: item N=%d status=%q, want %q", n, byN[n], w)
		}
		if byN[n] == engine.ItemPassed {
			survivors++
		}
	}
	if survivors != 1 {
		t.Errorf("prune_partial_crash: %d survivors after resume, want exactly 1 (keep: 1 — the frontier must not be re-derived against a partial score set)", survivors)
	}

	// Commit-once: the resumed run emits exactly one disposition per item (one
	// atomic frontier of 4 items), never a duplicate.
	for n, count := range dispositionCountByN(mustFoldEvents(t, h)) {
		if count != 1 {
			t.Errorf("prune_partial_crash: item N=%d committed %d times, want 1", n, count)
		}
	}
}

func testPruneQuorumCohort(t *testing.T, factory BackendFactory) {
	t.Helper()
	// prune: + reduce:{quorum} composition (the cohort-denominator fix). keep:
	// 2 over 4 items prunes the two lowest scorers; a quorum: 1.0 reduce then
	// folds the SURVIVORS' `agree` votes. The quorum threshold must be measured
	// against the NON-PRUNED cohort (2 survivors), exactly as min_success excludes
	// pruned items. The two survivors (b=0.9, d=0.7) both agree, so the unanimous
	// quorum is met (need 2, agree 2) and the map ends ok with votes=2. Measuring
	// quorum against the full fan-out (len(over)=4) — the old behavior — would
	// require 4 agreeing votes that the pruned items can never supply, wrongly
	// failing the map. The pruned items (a,c) agree=false, but they are absent from
	// the reducer's branches entirely, so only the cohort denominator can be wrong.
	programs := []execProgram{
		scoreAgreeProgram("a", 0.1, false), // pruned (lowest)
		scoreAgreeProgram("b", 0.9, true),  // survivor — agrees
		scoreAgreeProgram("c", 0.5, false), // pruned
		scoreAgreeProgram("d", 0.7, true),  // survivor — agrees
	}
	h := newHarnessWithInput(t, preProgramFake(t, factory, programs), pruneQuorumWorkflow,
		map[string]any{"items": []any{"a", "b", "c", "d"}})

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("prune_quorum_cohort: runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("prune_quorum_cohort: outcome = %q, want ok (unanimous survivors meet quorum over the NON-pruned cohort)", oc)
	}

	// The reduced verdict committed at the map path reports the cohort denominator:
	// votes must be 2 (the survivor count), NOT 4 (the full fan-out) — the proof
	// that pruned items leave the quorum cohort.
	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("prune_quorum_cohort: Fold: %v", ferr)
	}
	nr, ok := rs.LookupCompleted("map[0]")
	if !ok {
		t.Fatalf("prune_quorum_cohort: no reduced verdict committed at map[0]")
	}
	if got := nr.Outputs["votes"]; got != float64(2) {
		t.Errorf("prune_quorum_cohort: votes = %v, want 2 (cohort excludes the 2 pruned items)", got)
	}
	if got := nr.Outputs["agree"]; got != float64(2) {
		t.Errorf("prune_quorum_cohort: agree = %v, want 2 (both survivors agree)", got)
	}
	if got := nr.Outputs["passed"]; got != true {
		t.Errorf("prune_quorum_cohort: passed = %v, want true", got)
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
