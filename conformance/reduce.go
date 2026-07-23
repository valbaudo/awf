package conformance

import (
	"fmt"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// reduceQuorumWorkflow — C2a (Task 12) quorum form. A map over 3 items, each
// body emitting a typed {vulnerable: bool} verdict; reduce: {quorum: 2, field:
// vulnerable} collapses the 3 branches into ONE — the reduce ALWAYS commits
// {vulnerable:<bool>,...} regardless of whether ≥ 2 branches voted
// vulnerable=true (jury-panel Task 1: a vote tally never mechanically fails). A
// downstream code step `gate` reads {{ step.scan.vulnerable }} — proving the
// reduced output (NOT the per-item array) is what step.<bodyId> resolves to once
// a reduce: is declared, and that the verdict is named after field:.
//
// concurrency: 1 (the in-mem fake's shared Blobs race when multiple
// output_schema bodies commit concurrently — the aggregation bucket's note).
var reduceQuorumWorkflow = fmt.Sprintf(`workflow: conformance-reduce-quorum
version: 1
input_schema:
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
        field: vulnerable
  - id: gate
    container: c0
    run: "./gate.sh {{ step.scan.vulnerable }}"
    retry: { attempts: 1 }
`, fakeImageDigest)

// reduceRunWorkflow — C2a (Task 12) author run: reducer form. A map over 2
// items, each body declaring a NAMED output_files artifact (leaf → /out/leaf.csv);
// reduce: { run: ./merge.sh, container: agg, ... } stages every branch's named
// artifact + a canonical-JSON manifest into the reducer's REQUIRED container
// `agg` (via SP1's CopyTo), runs ./merge.sh, and commits its typed output +
// artifact at the map's OWN path. A downstream `collect` step stages the
// reducer's artifact via input_files step.row.files.csv, even though the body
// step itself declares only leaf — proving step.<bodyId>.files.<reducer-name>
// resolves to the REDUCER's artifact (Task 11 Step 6).
var reduceRunWorkflow = fmt.Sprintf(`workflow: conformance-reduce-run
version: 1
input_schema:
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
          output_files: { leaf: /out/leaf.csv }
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

// reduceNamedRunWorkflow is the named aggregate product variant of
// reduceRunWorkflow. It preserves the old body-step fixture above and proves the
// new `map.id` surface: downstream input_files reads
// step.<map-id>.files.<name>, and resume re-stages that artifact from the
// reducer's committed map-path node.completed record.
var reduceNamedRunWorkflow = fmt.Sprintf(`workflow: conformance-reduce-named-run
version: 1
input_schema:
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
      id: version_universe
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
        output_files: { files: /out/versions.csv }
  - id: collect
    container: c0
    run: "./collect.sh"
    retry: { attempts: 1 }
    input_files: { /work/versions.csv: step.version_universe.files.files }
`, fakeImageDigest)

// testReduce is the C2a conformance bucket (Task 12): reduce: fan-in on map.
// Three sub-tests pin the end-to-end behaviour against the fake backend:
//
//   - quorum_pass: a quorum that is MET collapses the per-item array into a
//     synthetic {<field>,votes,agree,votes_detail} typed output committed at the
//     map path; a downstream step.<bodyId>.<field> resolves to the REDUCED output.
//   - quorum_fail: a quorum that is NOT met still COMMITS {<field>:false,...} and
//     returns ok (jury-panel Task 1: a vote tally is never a mechanical failure);
//     the downstream step runs and sees the reduced verdict.
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
	t.Run("named_run_reduce_resume_artifact", func(t *testing.T) {
		testReduceNamedRunResumeArtifact(t, factory)
	})
	t.Run("map_gate_reduce", func(t *testing.T) { testMapGateReduce(t, factory) }) // NEW
	t.Run("map_gate_reduce_loud_missing", func(t *testing.T) { testMapGateReduceLoudMissing(t, factory) })
	t.Run("map_gate_reduce_resume", func(t *testing.T) { testMapGateReduceResume(t, factory) })
}

func testReduceQuorumPass(t *testing.T, factory BackendFactory) {
	t.Helper()
	mkScan := func(v bool) container.ExecResult {
		return container.ExecResult{ExitCode: 0, AWFOutput: []byte(fmt.Sprintf(`{"vulnerable":%t}`, v))}
	}
	// 2 of 3 items vote vulnerable=true → quorum: 2 MET → {vulnerable:true}. The
	// gate step's command renders {{ step.scan.vulnerable }} → "true", so it is
	// programmed as "./gate.sh true"; if the resolver lifted the per-item ARRAY
	// (the bug), the rendered command would differ and the fake would error
	// (unprogrammed).
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
	if nr.Outputs["vulnerable"] != true {
		t.Errorf("quorum_pass: vulnerable = %v, want true", nr.Outputs["vulnerable"])
	}
	if nr.Outputs["votes"] != float64(3) {
		t.Errorf("quorum_pass: votes = %v, want 3", nr.Outputs["votes"])
	}
	if nr.Outputs["agree"] != float64(2) {
		t.Errorf("quorum_pass: agree = %v, want 2", nr.Outputs["agree"])
	}
	vd, ok := nr.Outputs["votes_detail"].([]any)
	if !ok || len(vd) != 3 {
		t.Errorf("quorum_pass: votes_detail = %v, want 3 ballots", nr.Outputs["votes_detail"])
	}

	// The downstream gate step saw {{ step.scan.vulnerable }} → "true" (the reduced
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
		t.Errorf("quorum_pass: downstream gate did not see {{ step.scan.vulnerable }} → \"true\"; calls = %+v", runFake.Calls)
	}
}

func testReduceQuorumFail(t *testing.T, factory BackendFactory) {
	t.Helper()
	mkScan := func(v bool) container.ExecResult {
		return container.ExecResult{ExitCode: 0, AWFOutput: []byte(fmt.Sprintf(`{"vulnerable":%t}`, v))}
	}
	// Only 1 of 3 votes vulnerable=true → quorum: 2 NOT met → the reduce now
	// COMMITS {vulnerable:false} and returns ok (A1: a vote tally never
	// mechanically fails). The downstream gate.sh renders {{ step.scan.vulnerable }}
	// → "false" and runs.
	programmed := preProgramFake(t, factory, []execProgram{
		{cmd: "./scan.sh a", res: mkScan(true)},
		{cmd: "./scan.sh b", res: mkScan(false)},
		{cmd: "./scan.sh c", res: mkScan(false)},
		{cmd: "./gate.sh false", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"ok":true}`)}},
	})
	h := newHarnessWithInput(t, programmed, reduceQuorumWorkflow,
		map[string]any{"items": []any{"a", "b", "c"}})

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("quorum_fail: runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("quorum_fail: outcome = %q, want ok (a missed quorum commits, does not fail)", oc)
	}
	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("quorum_fail: Fold: %v", ferr)
	}
	nr, ok := rs.LookupCompleted("map[0]")
	if !ok {
		t.Fatalf("quorum_fail: no node.completed at map path (a missed quorum must still commit)")
	}
	if nr.Outputs["vulnerable"] != false {
		t.Errorf("quorum_fail: vulnerable = %v, want false", nr.Outputs["vulnerable"])
	}
	if nr.Outputs["agree"] != float64(1) {
		t.Errorf("quorum_fail: agree = %v, want 1", nr.Outputs["agree"])
	}
}

func testReduceRun(t *testing.T, factory BackendFactory) {
	t.Helper()
	merged := []byte("a-row\nb-row\n")

	// Round 1: program each item's row.sh to PRODUCE /out/leaf.csv (the named
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
				map[string][]byte{"/out/leaf.csv": []byte("a-row")})
			f.ProgramExecWithFiles("./row.sh b", container.ExecResult{ExitCode: 0}, nil,
				map[string][]byte{"/out/leaf.csv": []byte("b-row")})
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

func testReduceNamedRunResumeArtifact(t *testing.T, _ BackendFactory) {
	t.Helper()
	merged := []byte("a-row\nb-row\n")

	var runFake, resumeFake *container.Fake
	var resumeSpy *assetCopyToSpy
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
			f.FailExecAfterN(3) // crash collect after row,row,merge have committed
			runFake = f
			return f
		}
		f.ProgramExec("./collect.sh", container.ExecResult{ExitCode: 0}, nil)
		resumeFake = f
		resumeSpy = newAssetCopyToSpy(f)
		return resumeSpy
	}, reduceNamedRunWorkflow, map[string]any{"items": []any{"a", "b"}})

	oc, _ := h.runWorkflow(t)
	if oc == "" {
		t.Fatal("named_run_reduce_resume_artifact: first run produced no outcome")
	}
	if oc == engine.OutcomeOK {
		t.Fatal("named_run_reduce_resume_artifact: first run unexpectedly ok; collect should crash after reducer commit")
	}

	rs, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("named_run_reduce_resume_artifact: Fold: %v", err)
	}
	reduced, ok := rs.LookupCompleted("map[0]")
	if !ok {
		t.Fatal("named_run_reduce_resume_artifact: reducer did not commit at map[0]")
	}
	ref, ok := reduced.Files["/out/versions.csv"]
	if !ok {
		t.Fatalf("named_run_reduce_resume_artifact: reducer Files missing /out/versions.csv: %v", reduced.Files)
	}
	gotBytes, err := h.blobs.Get(ref)
	if err != nil {
		t.Fatalf("named_run_reduce_resume_artifact: Blobs.Get(%q): %v", ref, err)
	}
	if string(gotBytes) != string(merged) {
		t.Fatalf("named_run_reduce_resume_artifact: reducer artifact = %q, want %q", gotBytes, merged)
	}
	if _, ok := rs.LookupCompleted("collect"); ok {
		t.Fatal("named_run_reduce_resume_artifact: collect committed in run 1; resume would not re-stage the named aggregate artifact")
	}

	preReduceCommits := countNodeCompleted(mustFoldEvents(t, h), "map[0]")
	oc2, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("named_run_reduce_resume_artifact: resume: %v", err)
	}
	if oc2 != engine.OutcomeOK {
		t.Fatalf("named_run_reduce_resume_artifact: resume outcome = %q, want ok", oc2)
	}
	if resumeFake == nil || resumeSpy == nil {
		t.Fatal("named_run_reduce_resume_artifact: resume did not mint the spy fake")
	}
	for _, c := range resumeFake.Calls {
		if c.Run != "./collect.sh" {
			t.Fatalf("named_run_reduce_resume_artifact: resume re-executed %q; row/merge should replay", c.Run)
		}
	}
	staged := resumeSpy.stagedByPath()
	if staged["/work/versions.csv"] != string(merged) {
		t.Fatalf("named_run_reduce_resume_artifact: staged /work/versions.csv = %q, want %q (all staged: %#v)",
			staged["/work/versions.csv"], merged, staged)
	}
	postReduceCommits := countNodeCompleted(mustFoldEvents(t, h), "map[0]")
	if postReduceCommits != preReduceCommits {
		t.Fatalf("named_run_reduce_resume_artifact: map[0] commits changed across resume: %d -> %d",
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

// mapGateReduceWorkflow — P1 forwarding. A map whose body is a single gate:
// generate writes a NAMED output_files artifact (leaf → /out/leaf.csv) and
// evaluate passes; reduce: { run: ./merge.sh } must receive every branch's
// accepted-attempt leaf at $AWF_STAGING_ROOT/branch-<N>/leaf. Proves a gate
// body's accepted attempt forwards its file into the fan-in (the prestige gap).
// concurrency:1 — the in-mem fake's shared Blobs race on concurrent
// output_schema commits.
var mapGateReduceWorkflow = fmt.Sprintf(`workflow: conformance-map-gate-reduce
version: 1
input_schema:
  type: object
  required: [items]
  additionalProperties: false
  properties:
    items: { type: array, items: { type: string } }
containers:
  c0: { image: %[1]s }
  agg: { image: %[1]s }
graph:
  - map:
      over: "{{ input.items }}"
      as: x
      container: c0
      concurrency: 1
      body:
        - gate:
            generate:
              - id: gen
                container: c0
                run: "./gen.sh {{ x }}"
                retry: { attempts: 1 }
                output_files: { leaf: /out/leaf.csv }
            evaluate:
              - id: check
                container: c0
                run: "./check.sh"
                retry: { attempts: 1 }
                output_schema:
                  type: object
                  additionalProperties: false
                  required: [passed]
                  properties: { passed: { type: boolean } }
            until: "{{ evaluate.passed }}"
            max_attempts: 1
      reduce:
        run: "./merge.sh"
        container: agg
        output_schema:
          type: object
          additionalProperties: false
          required: [rows]
          properties: { rows: { type: integer } }
        output_files: { merged: /out/merged.csv }
`, fakeImageDigest)

// testMapGateReduceLoudMissing — item "a" produces its leaf and passes; item
// "b"'s gen exits 0 but never writes /out/leaf.csv, so output_files capture
// fails → the gate's generate is retryable→terminal → item b is a VISIBLE
// ItemFailed. The map (run: reducer) reduces over the survivor and returns ok;
// b's leaf is NOT staged and b is recorded failed — the opposite of the
// prestige glob that silently merged fewer files.
func testMapGateReduceLoudMissing(t *testing.T, _ BackendFactory) {
	t.Helper()
	var spy *assetCopyToSpy
	h := newHarnessWithInput(t, func() container.Backend {
		f := container.NewFake()
		f.ProgramExecWithFiles("./gen.sh a", container.ExecResult{ExitCode: 0}, nil,
			map[string][]byte{"/out/leaf.csv": []byte("a-leaf")})
		f.ProgramExec("./gen.sh b", container.ExecResult{ExitCode: 0}, nil) // exits 0, writes NO leaf
		f.ProgramExec("./check.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"passed":true}`)}, nil)
		f.ProgramExecWithFiles("./merge.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"rows":1}`)}, nil,
			map[string][]byte{"/out/merged.csv": []byte("a-leaf\n")})
		spy = newAssetCopyToSpy(f)
		return spy
	}, mapGateReduceWorkflow, map[string]any{"items": []any{"a", "b"}})

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("loud_missing: runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("loud_missing: outcome = %q, want ok (reducer succeeds over survivor a)", oc)
	}
	assertExactlyOneStagedPath(t, spy, "/work/.awf/branch-0/leaf", []byte("a-leaf"))
	assertNoStagedPath(t, spy, "/work/.awf/branch-1/leaf")

	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("loud_missing: Fold: %v", ferr)
	}
	var bStatus string
	for _, it := range rs.LookupMapItems("map[0]") {
		if it.N == 1 {
			bStatus = it.Status
		}
	}
	if bStatus != engine.ItemFailed {
		t.Fatalf("loud_missing: item b status = %q, want %q (declared output not produced must be auditable)", bStatus, engine.ItemFailed)
	}
}

// testMapGateReduceResume — round 1 runs the 2 items + their gates, then CRASHES
// on the reducer's merge.sh (FailExecAfterN before the reduce exec) → reduce
// uncommitted. Round 2 resumes against a fake that programs ONLY merge.sh: the
// items+gates replay from the journal (no re-exec), and collectReduceBranches
// re-runs against folded GateAttempts to re-stage both leaves. Proves the
// accepted-attempt resolution is resume-stable AND committed items do not re-run.
//
// Exec order (concurrency:1): gen a(0), check(1), gen b(2), check(3), merge(4).
// FailExecAfterN(4) lets the first 4 calls (0–3) succeed and fails call 4
// (merge.sh), crashing round 1 before the reduce commits.
func testMapGateReduceResume(t *testing.T, _ BackendFactory) {
	t.Helper()
	var runFake, resumeFake *container.Fake
	var resumeSpy *assetCopyToSpy
	h := newHarnessWithInput(t, func() container.Backend {
		f := container.NewFake()
		if runFake == nil {
			f.ProgramExecWithFiles("./gen.sh a", container.ExecResult{ExitCode: 0}, nil,
				map[string][]byte{"/out/leaf.csv": []byte("a-leaf")})
			f.ProgramExecWithFiles("./gen.sh b", container.ExecResult{ExitCode: 0}, nil,
				map[string][]byte{"/out/leaf.csv": []byte("b-leaf")})
			f.ProgramExec("./check.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"passed":true}`)}, nil)
			// Exec order (concurrency 1): gen a(0), check(1), gen b(2), check(3), merge(4).
			// Fail the 5th call (index 4) → crash before the reducer commits.
			f.FailExecAfterN(4)
			runFake = f
			return f
		}
		// Resume fake: program ONLY merge.sh. Items+gates must replay (no re-exec);
		// only the reducer re-runs.
		f.ProgramExecWithFiles("./merge.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"rows":2}`)}, nil,
			map[string][]byte{"/out/merged.csv": []byte("a-leaf\nb-leaf\n")})
		resumeFake = f
		resumeSpy = newAssetCopyToSpy(f)
		return resumeSpy
	}, mapGateReduceWorkflow, map[string]any{"items": []any{"a", "b"}})

	if _, err := h.runWorkflow(t); err == nil {
		t.Fatalf("map_gate_reduce_resume: round 1 expected a crash before the reducer, got nil error")
	}
	oc, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("map_gate_reduce_resume: round 2: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("map_gate_reduce_resume: outcome = %q, want ok", oc)
	}
	// collectReduceBranches re-ran on resume and re-staged both gate leaves.
	assertExactlyOneStagedPath(t, resumeSpy, "/work/.awf/branch-0/leaf", []byte("a-leaf"))
	assertExactlyOneStagedPath(t, resumeSpy, "/work/.awf/branch-1/leaf", []byte("b-leaf"))
	// Committed items did NOT re-run: the resume fake saw only the reducer.
	if resumeFake == nil {
		t.Fatal("map_gate_reduce_resume: resume did not mint a second fake")
	}
	for _, c := range resumeFake.Calls {
		if c.Run != "./merge.sh" {
			t.Fatalf("map_gate_reduce_resume: resume re-ran a committed step: %q (only ./merge.sh should run)", c.Run)
		}
	}
}

// testMapGateReduce proves each gate branch's accepted-attempt leaf is staged
// into the reducer. A faked merge.sh return alone would not distinguish 0 vs 2
// collected branches, so we wrap the fake in assetCopyToSpy (assets_stage.go:258)
// and assert on what the reducer received.
func testMapGateReduce(t *testing.T, _ BackendFactory) {
	t.Helper()
	var spy *assetCopyToSpy
	h := newHarnessWithInput(t, func() container.Backend {
		f := container.NewFake()
		f.ProgramExecWithFiles("./gen.sh a", container.ExecResult{ExitCode: 0}, nil,
			map[string][]byte{"/out/leaf.csv": []byte("a-leaf")})
		f.ProgramExecWithFiles("./gen.sh b", container.ExecResult{ExitCode: 0}, nil,
			map[string][]byte{"/out/leaf.csv": []byte("b-leaf")})
		f.ProgramExec("./check.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"passed":true}`)}, nil)
		f.ProgramExecWithFiles("./merge.sh", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"rows":2}`)}, nil,
			map[string][]byte{"/out/merged.csv": []byte("a-leaf\nb-leaf\n")})
		spy = newAssetCopyToSpy(f)
		return spy
	}, mapGateReduceWorkflow, map[string]any{"items": []any{"a", "b"}})

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("map_gate_reduce: runWorkflow: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("map_gate_reduce: outcome = %q, want ok", oc)
	}
	// Fake StagingRoot mirrors Docker: "/work/.awf". branch-<N> uses the item index.
	assertExactlyOneStagedPath(t, spy, "/work/.awf/branch-0/leaf", []byte("a-leaf"))
	assertExactlyOneStagedPath(t, spy, "/work/.awf/branch-1/leaf", []byte("b-leaf"))
}
