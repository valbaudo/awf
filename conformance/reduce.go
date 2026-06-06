package conformance

import (
	"fmt"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// reduceQuorumWorkflow — C2a (Task 12) quorum form. A map over 3 items, each
// body emitting a typed {vulnerable: bool} verdict; reduce: {quorum: 2, over:
// vulnerable} collapses the 3 branches into ONE — the node succeeds iff ≥ 2
// branches voted vulnerable=true. A downstream code step `gate` reads
// {{ step.scan.passed }} — proving the reduced output (NOT the per-item array)
// is what step.<bodyId> resolves to once a reduce: is declared.
//
// concurrency: 1 (the in-mem fake's shared Blobs race when multiple
// output_schema bodies commit concurrently — the aggregation bucket's note).
var reduceQuorumWorkflow = fmt.Sprintf(`workflow: conformance-reduce-quorum
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
        - id: scan
          container: c0
          run: "./scan.sh {{ x }}"
          retry: { attempts: 1 }
          output_schema:
            type: object
            additionalProperties: false
            required: [vulnerable]
            properties:
              vulnerable: { type: boolean }
      reduce:
        quorum: 2
        over: vulnerable
  - id: gate
    container: c0
    run: "./gate.sh {{ step.scan.passed }}"
    retry: { attempts: 1 }
`, fakeImageDigest)

// reduceRunWorkflow — C2a (Task 12) author run: reducer form. A map over 2
// items, each body declaring a NAMED output_files artifact (row → /out/row.csv);
// reduce: { run: ./merge.sh, container: agg, ... } stages every branch's named
// artifact + a canonical-JSON manifest into the reducer's REQUIRED container
// `agg` (via SP1's CopyTo), runs ./merge.sh, and commits its typed output +
// artifact at the map's OWN path. A downstream `collect` step stages the
// reducer's artifact via input_files step.row.files.csv — proving
// step.<bodyId>.files.<name> resolves to the REDUCER's artifact (Task 11 Step 6).
var reduceRunWorkflow = fmt.Sprintf(`workflow: conformance-reduce-run
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
    image: %[1]s
  agg:
    image: %[1]s
graph:
  - map:
      over: "{{ input.items }}"
      as: x
      container: c0
      concurrency: 1
      body:
        - id: row
          container: c0
          run: "./row.sh {{ x }}"
          retry: { attempts: 1 }
          output_files: { csv: /out/versions.csv }
      reduce:
        run: "./merge.sh"
        container: agg
        output_schema:
          type: object
          additionalProperties: false
          required: [csv_rows]
          properties:
            csv_rows: { type: integer }
        output_files: { csv: /out/versions.csv }
  - id: collect
    container: c0
    run: "./collect.sh"
    retry: { attempts: 1 }
    input_files: { /work/versions.csv: step.row.files.csv }
`, fakeImageDigest)

// testReduce is the C2a conformance bucket (Task 12): reduce: fan-in on map.
// Three sub-tests pin the end-to-end behaviour against the fake backend:
//
//   - quorum_pass: a quorum that is MET collapses the per-item array into a
//     synthetic {passed,votes,agree} typed output committed at the map path; a
//     downstream step.<bodyId>.passed resolves to the REDUCED output.
//   - quorum_fail: a quorum that is NOT met returns retryable_failure (mirrors
//     min_success) and never commits at the map path; the run halts mechanically.
//   - run_reduce: an author shell reducer stages every branch's named artifact +
//     a manifest into its required container, commits its typed output + artifact
//     at the map path; a downstream step resolves step.<bodyId>.files.<name> to
//     the REDUCER's artifact. A post-completion resume replays the reduced node
//     (no re-exec) and the map-path node.completed count is unchanged.
func testReduce(t *testing.T, factory BackendFactory) {
	t.Helper()
	t.Run("quorum_pass", func(t *testing.T) { testReduceQuorumPass(t, factory) })
	t.Run("quorum_fail", func(t *testing.T) { testReduceQuorumFail(t, factory) })
	t.Run("run_reduce", func(t *testing.T) { testReduceRun(t, factory) })
}

func testReduceQuorumPass(t *testing.T, factory BackendFactory) {
	t.Helper()
	mkScan := func(v bool) container.ExecResult {
		return container.ExecResult{ExitCode: 0, AWFOutput: []byte(fmt.Sprintf(`{"vulnerable":%t}`, v))}
	}
	// 2 of 3 items vote vulnerable=true → quorum: 2 MET → {passed:true}. The gate
	// step's command renders {{ step.scan.passed }} → "true", so it is programmed
	// as "./gate.sh true"; if the resolver lifted the per-item ARRAY (the bug),
	// the rendered command would differ and the fake would error (unprogrammed).
	var runFake *container.Fake
	capturing := func() container.Backend {
		b := factory()
		fake, ok := b.(*container.Fake)
		if !ok {
			return b
		}
		fake.ProgramExec("./scan.sh a", mkScan(true), nil)
		fake.ProgramExec("./scan.sh b", mkScan(true), nil)
		fake.ProgramExec("./scan.sh c", mkScan(false), nil)
		fake.ProgramExec("./gate.sh true", container.ExecResult{ExitCode: 0}, nil)
		runFake = fake
		return fake
	}
	h := newHarnessWithInput(t, capturing, reduceQuorumWorkflow,
		map[string]any{"items": []any{"a", "b", "c"}})

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("quorum_pass: runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("quorum_pass: outcome = %q, want ok", oc)
	}

	// The reducer committed a node.completed at the map path with the synthetic
	// quorum verdict. Fold the log and look up the bare map path.
	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("quorum_pass: Fold: %v", ferr)
	}
	nr, ok := rs.LookupCompleted("map[0]")
	if !ok {
		t.Fatalf("quorum_pass: no node.completed at map[0] (the reduced output)")
	}
	if nr.Outputs["passed"] != true {
		t.Errorf("quorum_pass: passed = %v, want true", nr.Outputs["passed"])
	}
	if nr.Outputs["votes"] != float64(3) {
		t.Errorf("quorum_pass: votes = %v, want 3", nr.Outputs["votes"])
	}
	if nr.Outputs["agree"] != float64(2) {
		t.Errorf("quorum_pass: agree = %v, want 2", nr.Outputs["agree"])
	}

	// The downstream gate step saw {{ step.scan.passed }} → "true" (the reduced
	// output, NOT the per-item array). It committed ok above; here we assert the
	// exact rendered command reached the fake.
	if runFake == nil {
		t.Skip("quorum_pass: backend is not *container.Fake; downstream-render proof is fake-only")
	}
	sawGate := false
	for _, c := range runFake.Calls {
		if c.Run == "./gate.sh true" {
			sawGate = true
		}
	}
	if !sawGate {
		t.Errorf("quorum_pass: downstream gate did not see {{ step.scan.passed }} → \"true\"; calls = %+v", runFake.Calls)
	}
}

func testReduceQuorumFail(t *testing.T, factory BackendFactory) {
	t.Helper()
	mkScan := func(v bool) container.ExecResult {
		return container.ExecResult{ExitCode: 0, AWFOutput: []byte(fmt.Sprintf(`{"vulnerable":%t}`, v))}
	}
	// Only 1 of 3 votes vulnerable=true → quorum: 2 NOT met → the map node
	// returns retryable_failure (no node.completed at map[0]) and the run halts.
	// gate.sh is deliberately NOT programmed: if the run reached it the
	// fake's ProgramExec-miss would fire — proving the run halted at the reduce.
	programmed := preProgramFake(t, factory, []execProgram{
		{cmd: "./scan.sh a", res: mkScan(true)},
		{cmd: "./scan.sh b", res: mkScan(false)},
		{cmd: "./scan.sh c", res: mkScan(false)},
	})
	// Reuse reduceQuorumWorkflow (quorum: 2): with only 1 of 3 voting true, the
	// same quorum: 2 that PASSED above now FAILS — same fixture, different data.
	h := newHarnessWithInput(t, programmed, reduceQuorumWorkflow,
		map[string]any{"items": []any{"a", "b", "c"}})

	oc, err := h.runWorkflow(t)
	if oc != engine.OutcomeRetryableFailure {
		t.Fatalf("quorum_fail: outcome = %q (err=%v), want retryable_failure", oc, err)
	}
	if err == nil {
		t.Fatalf("quorum_fail: err = nil, want a non-nil missed-quorum propagation")
	}

	// No node.completed at the map path — a not-met quorum must not commit
	// (mirrors min_success).
	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("quorum_fail: Fold: %v", ferr)
	}
	if _, ok := rs.LookupCompleted("map[0]"); ok {
		t.Errorf("quorum_fail: a not-met quorum committed at map[0]; must not (mirrors min_success)")
	}
	// gate (the downstream step) must NOT have committed — the run halted.
	if _, ok := rs.LookupCompleted("gate"); ok {
		t.Errorf("quorum_fail: downstream gate committed; the run should have halted at the reduce")
	}
}

func testReduceRun(t *testing.T, factory BackendFactory) {
	t.Helper()
	merged := []byte("a-row\nb-row\n")

	// Round 1: program each item's row.sh to PRODUCE /out/row.csv (the named
	// artifact, via the exec-produces-files affordance) and the reducer's
	// merge.sh to read the staged manifest+artifacts and emit {csv_rows:2} + write
	// /out/versions.csv. Round 2 resumes against a BARE fake (no programmed Exec):
	// if the reduced node re-executed it would error, so a clean resume proves it
	// REPLAYS from the journal.
	var runFake, resumeFake *container.Fake
	h := newHarnessWithInput(t, func() container.Backend {
		f := container.NewFake()
		if runFake == nil {
			f.ProgramExecWithFiles("./row.sh a", container.ExecResult{ExitCode: 0}, nil,
				map[string][]byte{"/out/versions.csv": []byte("a-row")})
			f.ProgramExecWithFiles("./row.sh b", container.ExecResult{ExitCode: 0}, nil,
				map[string][]byte{"/out/versions.csv": []byte("b-row")})
			f.ProgramExecWithFiles("./merge.sh", container.ExecResult{
				ExitCode:  0,
				AWFOutput: []byte(`{"csv_rows":2}`),
			}, nil, map[string][]byte{"/out/versions.csv": merged})
			f.ProgramExec("./collect.sh", container.ExecResult{ExitCode: 0}, nil)
			runFake = f
		} else {
			// Resume fake: NOTHING programmed. Every committed node — the 2 items,
			// the reduce, and collect — must replay from the journal; any re-exec
			// errors against this bare fake and fails the resume assertion.
			resumeFake = f
		}
		return f
	}, reduceRunWorkflow, map[string]any{"items": []any{"a", "b"}})

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run_reduce: runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("run_reduce: outcome = %q, want ok", oc)
	}

	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("run_reduce: Fold: %v", ferr)
	}

	// The reducer committed its typed output + artifact at the map path.
	nr, ok := rs.LookupCompleted("map[0]")
	if !ok {
		t.Fatalf("run_reduce: no node.completed at map[0] (the reduced output)")
	}
	if nr.Outputs["csv_rows"] != float64(2) {
		t.Errorf("run_reduce: csv_rows = %v, want 2", nr.Outputs["csv_rows"])
	}
	csvRef, ok := nr.Files["/out/versions.csv"]
	if !ok {
		t.Fatalf("run_reduce: reducer artifact /out/versions.csv not committed; Files = %v", nr.Files)
	}
	gotBytes, gerr := h.blobs.Get(csvRef)
	if gerr != nil {
		t.Fatalf("run_reduce: Blobs.Get(%q): %v", csvRef, gerr)
	}
	if string(gotBytes) != string(merged) {
		t.Errorf("run_reduce: reducer artifact bytes = %q, want %q", gotBytes, merged)
	}

	// The downstream collect step committed ok — its input_files
	// step.row.files.csv resolved to the REDUCER's artifact (Task 11 Step 6) and
	// staged via CopyTo. Had the resolver descended to the per-item path, the
	// staging ref would be the wrong (per-item) blob, but collect would still
	// commit; the load-bearing proof of "reducer's artifact" is the byte-exact
	// assertion above plus the resolver short-circuit. Here we confirm the
	// reference resolved at all (collect committed).
	if _, ok := rs.LookupCompleted("collect"); !ok {
		t.Fatalf("run_reduce: collect not committed; step.row.files.csv failed to resolve the reducer artifact")
	}

	// Count map-path node.completed events before resume.
	preReduceCommits := countNodeCompleted(mustFoldEvents(t, h), "map[0]")
	if preReduceCommits != 1 {
		t.Fatalf("run_reduce: map[0] node.completed count = %d, want 1 after round 1", preReduceCommits)
	}

	// Resume against the bare fake: every committed node replays, nothing
	// re-executes (the resume fake has no programmed Exec). The run completes ok.
	oc2, err2 := h.resumeWorkflow(t)
	if err2 != nil {
		t.Fatalf("run_reduce: resume: %v (the reduced node must replay, not re-execute)", err2)
	}
	if oc2 != engine.OutcomeOK {
		t.Fatalf("run_reduce: resume outcome = %q, want ok", oc2)
	}
	if resumeFake == nil {
		t.Fatal("run_reduce: resume did not mint a second fake")
	}
	for _, c := range resumeFake.Calls {
		t.Errorf("run_reduce: resume re-executed %q; a committed reduce must replay, not recompute", c.Run)
	}

	// The map-path node.completed count is unchanged — replay does not re-commit.
	postReduceCommits := countNodeCompleted(mustFoldEvents(t, h), "map[0]")
	if postReduceCommits != preReduceCommits {
		t.Errorf("run_reduce: map[0] node.completed count changed across resume: %d → %d (replay must not re-commit)",
			preReduceCommits, postReduceCommits)
	}
}

// countNodeCompleted folds-side helper: counts node.completed events at exactly
// path. Used to prove the reduced node commits once and replays (does not
// re-commit) across resume.
func countNodeCompleted(events []state.Event, path string) int {
	n := 0
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted && e.Path == path {
			n++
		}
	}
	return n
}
