// Package conformance is the Backend-parameterized test suite the design
// spec §H calls "the definition of done" for Phase 2 onward.
//
//   - Pinning (Bucket 1): a workflow file mutated between run and resume
//     is a hard error; the run is NOT advanced.
//   - Exact committed-prefix replay (Bucket 2): on resume, committed steps
//     are replayed from the journal (NOT re-executed); the resumed
//     RunState.Completed[step] byte-equals the original.
//   - Atomic commit (Bucket 3): a crash between Blobs.Put and
//     Log.Append(node.completed) leaves orphan blobs but no log entry;
//     every node.completed references a present blob.
//   - Propagation (Bucket 4): try.catch absorbs a retry-exhausted failure
//     (caught); without try the failure propagates (uncaught). Slice 3.2
//     adds: a parallel with a failing branch has siblings observe
//     ctx-cancel + run finally blocks + propagate to enclosing try
//     (parallel_cancellation); a mid-parallel crash resumes with committed
//     branches replayed and uncommitted re-executed (parallel_resume_consistency).
//   - Gate (Bucket 5): a gate with max_attempts:2 threads evaluator
//     feedback into the next attempt's generator (feedback_threading);
//     a gate whose evaluator always rejects exhausts max_attempts and
//     returns OutcomeRejected (max_attempts_rejected); a generator
//     crash propagates BEFORE any gate.attempt commits, never
//     consuming an attempt (crash_not_verdict); a mid-gate resume
//     continues at the committed attempt N+1 (mid_resume); each step
//     in the gate is dispatched independently (independence_placeholder —
//     Phase 5 replaces with fresh-context agent-launch proof); gate
//     rejection propagates to the nearest try.catch (rejected_caught_by_try).
//   - Skip (Bucket 6): skip at run root completes ok with node.skipped in
//     log (at_root); skip in loop body records loop.iter + node.skipped per
//     iter (in_loop_body); skip in try.do bypasses Catch, runs Finally,
//     propagates ok (in_try_do).
//   - Map (Bucket 7): a 3-item map commits 3 distinct map.item events at
//     independently-addressable per-item paths (map_per_item_commits);
//     round-1 commits all 3 items, round-2 resumes against a BARE fake
//     (no programmed Exec) and completes ok — committed items are
//     REPLAYED, not re-executed (map_resume_skips_committed_items); skip
//     inside an item commits item_passed (map_skip_in_item_records_passed —
//     pins design §E step 5).
//   - Signal (Bucket 8): a signal written before the run starts is
//     consumed at the await on first poll (signal_await_delivers); a
//     committed signal.received + node.completed pair replays cleanly
//     on resume (signal_resume_replays); pause.json halts the run at the
//     next commit boundary with run.paused appended (signal_pause_halts);
//     cancel.json appends terminal run.cancelled + returns ErrCancelled
//     (signal_cancel_terminal).
//   - Obs (Bucket 16): obs.Project over an obs-owned fake-backend run's folded
//     log is a deterministic read-only projection — span tree mirrors the
//     engine/path addressing tree bidirectionally (span_tree_mirrors_addressing);
//     two projections of the same log are DeepEqual (byte_identical_replay); an
//     unfinalized node.started yields a Pending span (truncated_log_pending); the
//     in-memory exporter round-trips a value (local_exporter_roundtrips); a
//     gate.attempt projects a gen_ai.evaluation.result event (gate_evaluation_result);
//     a parallel of agent steps rolls up awf.run.cost.usd over leaves only, never
//     scope spans (cost_rollup_scope_not_summed).
//
// Phase 2 calls RunSuite with container.NewFake (conformance_fake_test.go).
// Slice 4.6 added RunDockerSuite + conformance_docker_test.go for Buckets
// 9/10/11 against real Docker; see conformance/docker_suite_test.go.
//
// Slice 2.6 Design question 1: bucket impls live in non-_test.go files
// so RunSuite can invoke them across the package boundary. Only
// conformance_fake_test.go is _test.go.
//
// Slice 2.6 Design question 3: state is in-mem throughout (InMemoryLog +
// InMemoryBlobs); the workflow YAML lives on disk because loader.Load
// needs a path. Phase 4 swaps only the Backend.
package conformance

import (
	"testing"

	"github.com/valbaudo/awf/container"
)

// BackendFactory mints a fresh container.Backend per "lifetime" — one for
// the first run, one for the resume. Models the spec §8 "containers
// reconstructed from image/compose recipe on every (re)creation" semantic.
type BackendFactory func() container.Backend

// RunSuite is the single entry point. Sub-tests run independently.
func RunSuite(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("pinning", func(t *testing.T) { testPinning(t, factory) })
	t.Run("replay", func(t *testing.T) { testReplay(t, factory) })
	t.Run("atomic", func(t *testing.T) { testAtomic(t, factory) })
	t.Run("propagation", func(t *testing.T) { testPropagation(t, factory) })
	t.Run("gate", func(t *testing.T) { testGate(t, factory) })
	t.Run("agent_step", func(t *testing.T) { testAgentStep(t, factory) })
	t.Run("gate_agent", func(t *testing.T) { testGateAgent(t, factory) })
	t.Run("layer2_contract", func(t *testing.T) { testLayer2Contract(t, factory) })
	t.Run("skip", func(t *testing.T) { testSkip(t, factory) })
	t.Run("map", func(t *testing.T) { testMap(t, factory) })
	t.Run("signal", func(t *testing.T) { testSignal(t, factory) })
	t.Run("obs", func(t *testing.T) { testObs(t, factory) })
	t.Run("snapshot", func(t *testing.T) { testSnapshot(t, factory) })
}
