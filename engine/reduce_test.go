package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// reduceRig bundles the dependencies runReduce takes. Mirrors mapRig but is
// reduce-focused: the run: form needs a programmable fake + a handle for the
// reducer's declared container.
type reduceRig struct {
	ld    *LocalDispatcher
	fake  *container.Fake
	clk   *clock.Fake
	lg    *state.InMemoryLog
	blobs *state.InMemoryBlobs
	rs    *RunState
}

const reduceContainer = "agg"

// newReduceRig builds a rig with a single reducer container (agg) handle and an
// empty RunState. The quorum form ignores the container; the run form uses it.
func newReduceRig(t *testing.T) *reduceRig {
	t.Helper()
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: reduceContainer})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	clk := &clock.Fake{T: testClockEpoch}
	return &reduceRig{
		ld:    &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{reduceContainer: h}},
		fake:  fake,
		clk:   clk,
		lg:    state.NewInMemoryLog(clk),
		blobs: state.NewInMemoryBlobs(),
		rs:    NewRunState(testRunID, testDigest, nil),
	}
}

// minimalReduceWorkflow builds a workflow with one map declaring `reduce` over
// a container so runCommandReduce's ContainerSpecFor resolves.
func minimalReduceWorkflow() *ir.Workflow {
	return &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{
			reduceContainer: {Image: "oci://example.com/r@sha256:" + strings.Repeat("0", 64)},
		},
		Graph: ir.NodeList{},
	}
}

func TestRunReduceQuorumMet(t *testing.T) {
	rig := newReduceRig(t)
	branches := []reduceBranch{
		{N: 0, Outputs: map[string]any{"vulnerable": true}},
		{N: 1, Outputs: map[string]any{"vulnerable": true}},
		{N: 2, Outputs: map[string]any{"vulnerable": false}},
	}
	q := ir.Ratio("2")
	r := &ir.Reduce{Quorum: &q, Over: "vulnerable"}

	oc, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), minimalReduceWorkflow(), rig.rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil)
	if err != nil {
		t.Fatalf("runReduce: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want ok", oc)
	}
	nr, ok := rig.rs.LookupCompleted(testMapPath)
	if !ok {
		t.Fatalf("no NodeResult committed at %q", testMapPath)
	}
	if nr.Outputs["passed"] != true {
		t.Errorf("passed = %v, want true", nr.Outputs["passed"])
	}
	if nr.Outputs["votes"] != 3 {
		t.Errorf("votes = %v, want 3", nr.Outputs["votes"])
	}
	if nr.Outputs["agree"] != 2 {
		t.Errorf("agree = %v, want 2", nr.Outputs["agree"])
	}
}

// TestQuorumReduceOutputMatchesVerdictFields pins the cross-package contract
// (SRP-4): runQuorumReduce must commit EXACTLY the keys ir.QuorumVerdictFields
// declares — the set the validator binds downstream refs against. If the engine
// adds/renames a verdict key without updating ir.QuorumVerdictFields (or vice
// versa), this fails, closing the producer/validator drift gap.
func TestQuorumReduceOutputMatchesVerdictFields(t *testing.T) {
	rig := newReduceRig(t)
	branches := []reduceBranch{{N: 0, Outputs: map[string]any{"vulnerable": true}}}
	q := ir.Ratio("1")
	r := &ir.Reduce{Quorum: &q, Over: "vulnerable"}

	if _, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), minimalReduceWorkflow(), rig.rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil); err != nil {
		t.Fatalf("runReduce: %v", err)
	}
	nr, ok := rig.rs.LookupCompleted(testMapPath)
	if !ok {
		t.Fatalf("no NodeResult committed at %q", testMapPath)
	}
	if len(nr.Outputs) != len(ir.QuorumVerdictFields) {
		t.Fatalf("quorum verdict %v has %d keys, want %d (ir.QuorumVerdictFields=%v)",
			nr.Outputs, len(nr.Outputs), len(ir.QuorumVerdictFields), ir.QuorumVerdictFields)
	}
	for k := range nr.Outputs {
		if !ir.QuorumVerdictFields[k] {
			t.Errorf("quorum verdict key %q absent from ir.QuorumVerdictFields %v — producer/validator drift",
				k, ir.QuorumVerdictFields)
		}
	}
}

func TestRunReduceQuorumNotMet(t *testing.T) {
	rig := newReduceRig(t)
	branches := []reduceBranch{
		{N: 0, Outputs: map[string]any{"vulnerable": true}},
		{N: 1, Outputs: map[string]any{"vulnerable": true}},
		{N: 2, Outputs: map[string]any{"vulnerable": false}},
	}
	q := ir.Ratio("3")
	r := &ir.Reduce{Quorum: &q, Over: "vulnerable"}

	oc, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), minimalReduceWorkflow(), rig.rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil)
	if oc != OutcomeRetryableFailure {
		t.Fatalf("Outcome = %q (err=%v), want retryable_failure", oc, err)
	}
	if err == nil {
		t.Fatalf("want a non-nil error explaining the missed quorum")
	}
	if _, ok := rig.rs.LookupCompleted(testMapPath); ok {
		t.Errorf("a not-met quorum must NOT commit a NodeResult at %q (mirrors min_success)", testMapPath)
	}
}

func TestRunReduceQuorumThresholdIsCohortNotSurvivors(t *testing.T) {
	// Regression: the quorum threshold k must be measured against the fan-out
	// COHORT, not the survivor count. With 3 items where 2 crashed mechanically
	// (absent from branches) and the 1 survivor agrees, a quorum: 2 must FAIL —
	// the author asked for 2 agreeing branches over a cohort of 3, and only 1
	// agrees. (The old code measured k against len(branches)=1 and the int-cap
	// `if i > total` silently lowered need to 1, vacuously passing.)
	rig := newReduceRig(t)
	branches := []reduceBranch{
		{N: 0, Outputs: map[string]any{"vulnerable": true}}, // the only survivor; agrees
		// items 1 and 2 crashed → no committed body output → absent.
	}
	q := ir.Ratio("2")
	r := &ir.Reduce{Quorum: &q, Over: "vulnerable"}

	oc, err := runReduce(context.Background(), r, testMapPath, branches, 3 /* cohort */, minimalReduceWorkflow(), rig.rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil)
	if oc != OutcomeRetryableFailure {
		t.Fatalf("quorum 2 over cohort 3 with 1 agreeing survivor: outcome = %q (err=%v), want retryable_failure", oc, err)
	}
	if err == nil {
		t.Fatal("want a non-nil error explaining the missed quorum")
	}
	if _, ok := rig.rs.LookupCompleted(testMapPath); ok {
		t.Errorf("a not-met quorum must NOT commit a NodeResult at %q", testMapPath)
	}
}

func TestRunReduceQuorumAllBranchesCrashedIsNotVacuousPass(t *testing.T) {
	// Regression: when EVERY branch crashes mechanically (branches empty) a
	// quorum must NOT vacuously pass with zero votes. quorum: 2 over a cohort of
	// 3 with 0 survivors → need=2, agree=0 → retryable_failure (the old code:
	// need=min(2,0)=0, agree=0 → passed with votes=0).
	rig := newReduceRig(t)
	q := ir.Ratio("2")
	r := &ir.Reduce{Quorum: &q, Over: "vulnerable"}

	oc, err := runReduce(context.Background(), r, testMapPath, nil /* all crashed */, 3 /* cohort */, minimalReduceWorkflow(), rig.rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil)
	if oc != OutcomeRetryableFailure {
		t.Fatalf("quorum 2 over cohort 3 with 0 survivors: outcome = %q (err=%v), want retryable_failure", oc, err)
	}
	if err == nil {
		t.Fatal("want a non-nil error explaining the missed quorum")
	}
	if _, ok := rig.rs.LookupCompleted(testMapPath); ok {
		t.Errorf("an all-crashed cohort must NOT vacuously commit a passed quorum at %q", testMapPath)
	}
}

func TestRunReduceResumeReplays(t *testing.T) {
	rig := newReduceRig(t)
	// Pre-seed a committed reduced result at the node path.
	rig.rs.RecordCompleted(testMapPath, NodeResult{Outcome: OutcomeOK, Outputs: map[string]any{"passed": true}})
	q := ir.Ratio("2")
	r := &ir.Reduce{Quorum: &q, Over: "vulnerable"}

	oc, err := runReduce(context.Background(), r, testMapPath, nil, 0, minimalReduceWorkflow(), rig.rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil)
	if err != nil {
		t.Fatalf("runReduce (resume): %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want ok (committed reduce replays)", oc)
	}
	// No new node.completed event should have been appended (the seed was
	// in-memory only; the log is empty, so any commit would show up).
	events, ferr := rig.lg.Fold()
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}
	for _, e := range events {
		if e.Type == EventNodeCompleted {
			t.Errorf("resume re-committed at %q (found a node.completed) — must replay, not recompute", e.Path)
		}
	}
}

func TestRunReduceCommandStagesManifestAndArtifacts(t *testing.T) {
	rig := newReduceRig(t)
	mapPath := testMapPath

	// Two branches, each with a committed artifact already in Blobs.
	row0, err := rig.blobs.Put([]byte("row-zero"))
	if err != nil {
		t.Fatalf("Put row0: %v", err)
	}
	row1, err := rig.blobs.Put([]byte("row-one"))
	if err != nil {
		t.Fatalf("Put row1: %v", err)
	}
	branches := []reduceBranch{
		{N: 0, Outputs: map[string]any{"k": "a"}, Files: map[string]string{"/out/row.csv": row0}},
		{N: 1, Outputs: map[string]any{"k": "b"}, Files: map[string]string{"/out/row.csv": row1}},
	}

	// Program the reducer command to produce a typed output + one artifact.
	rig.fake.ProgramExecWithFiles("./merge.sh", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"csv_rows":2}`),
	}, nil, map[string][]byte{"/out/versions.csv": []byte("merged-bytes")})

	schema := ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"csv_rows"},
		"properties":           map[string]any{"csv_rows": map[string]any{"type": "integer"}},
	}
	r := &ir.Reduce{
		Run:          "./merge.sh",
		Container:    reduceContainer,
		OutputSchema: &schema,
		OutputFiles:  ir.OutputFiles{{Name: "csv", Path: "/out/versions.csv"}},
	}

	oc, err := runReduce(context.Background(), r, mapPath, branches, len(branches), minimalReduceWorkflow(), rig.rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil)
	if err != nil {
		t.Fatalf("runReduce (run): %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want ok", oc)
	}

	// The reducer's container received the manifest + each branch's artifact.
	h := rig.ld.Handles[reduceContainer]
	captured, cerr := rig.fake.CaptureFiles(context.Background(), h, []string{
		reduceManifestPath,
		"/work/.awf/branch-0/out/row.csv",
		"/work/.awf/branch-1/out/row.csv",
	})
	if cerr != nil {
		t.Fatalf("CaptureFiles: %v", cerr)
	}
	// Manifest is canonical JSON of the index-ordered branch outputs.
	var manifest []map[string]any
	if jerr := json.Unmarshal(captured[0].Content, &manifest); jerr != nil {
		t.Fatalf("manifest unmarshal: %v (raw=%s)", jerr, captured[0].Content)
	}
	if len(manifest) != 2 || manifest[0]["k"] != "a" || manifest[1]["k"] != "b" {
		t.Errorf("manifest = %v, want index-ordered [{k:a},{k:b}]", manifest)
	}
	if string(captured[1].Content) != "row-zero" {
		t.Errorf("branch-0 artifact = %q, want row-zero", captured[1].Content)
	}
	if string(captured[2].Content) != "row-one" {
		t.Errorf("branch-1 artifact = %q, want row-one", captured[2].Content)
	}

	// The node committed the reducer's typed output + artifact at the node path.
	nr, ok := rig.rs.LookupCompleted(mapPath)
	if !ok {
		t.Fatalf("no NodeResult committed at %q", mapPath)
	}
	if nr.Outputs["csv_rows"] != float64(2) {
		t.Errorf("csv_rows = %v, want 2", nr.Outputs["csv_rows"])
	}
	ref, ok := nr.Files["/out/versions.csv"]
	if !ok {
		t.Fatalf("no committed artifact at /out/versions.csv")
	}
	got, gerr := rig.blobs.Get(ref)
	if gerr != nil {
		t.Fatalf("Get reducer artifact: %v", gerr)
	}
	if string(got) != "merged-bytes" {
		t.Errorf("reducer artifact bytes = %q, want merged-bytes", got)
	}
}

func TestRunReduceTemplatesBodyStepRefsAsJSON(t *testing.T) {
	rig := newReduceRig(t)
	mapPath := "map[0]"
	rowSchema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"k"},
		"properties":           map[string]any{"k": map[string]any{"type": "string"}},
	}
	reduceSchema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"csv_rows"},
		"properties":           map[string]any{"csv_rows": map[string]any{"type": "integer"}},
	}
	wf := minimalReduceWorkflow()
	wf.Graph = ir.NodeList{
		&ir.Map{
			Over:        ir.Expr("{{ input.items }}"),
			As:          "x",
			Container:   reduceContainer,
			Concurrency: 1,
			Body: ir.NodeList{
				&ir.CodeStep{ID: "scan", Run: "./scan {{ x }}", Container: reduceContainer, OutputSchema: rowSchema},
			},
		},
	}
	branches := []reduceBranch{
		{N: 0, Outputs: map[string]any{"k": "a"}},
		{N: 1, Outputs: map[string]any{"k": "b"}},
	}
	rig.rs.RecordMapItem(mapPath, MapItemRecord{N: 0, ItemValue: "a", Status: ItemPassed})
	rig.rs.RecordMapItem(mapPath, MapItemRecord{N: 1, ItemValue: "b", Status: ItemPassed})
	rig.rs.RecordCompleted(ItemStepPath(mapPath, 0, "scan"), NodeResult{Outcome: OutcomeOK, Outputs: map[string]any{"k": "a"}})
	rig.rs.RecordCompleted(ItemStepPath(mapPath, 1, "scan"), NodeResult{Outcome: OutcomeOK, Outputs: map[string]any{"k": "b"}})

	const rendered = `["a","b"]`
	outputPath := `/out/["a","b"].json`
	rig.fake.ProgramExecWithFiles("./merge.sh "+rendered, container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"csv_rows":2}`),
	}, nil, map[string][]byte{outputPath: []byte("merged-bytes")})

	r := &ir.Reduce{
		Run:          "./merge.sh {{ step.scan.k }}",
		Container:    reduceContainer,
		OutputSchema: reduceSchema,
		OutputFiles:  ir.OutputFiles{{Name: "json", Path: "/out/{{ step.scan.k }}.json"}},
	}
	oc, err := runReduce(context.Background(), r, mapPath, branches, len(branches), wf, rig.rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil)
	if err != nil {
		t.Fatalf("runReduce: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want ok", oc)
	}
	nr, ok := rig.rs.LookupCompleted(mapPath)
	if !ok {
		t.Fatalf("no NodeResult committed at %q", mapPath)
	}
	if _, ok := nr.Files[outputPath]; !ok {
		t.Fatalf("no committed artifact at %q (files=%v)", outputPath, nr.Files)
	}
}

// A run: reducer's command must be TEMPLATED against the map-path scope, exactly
// like a code step's run — otherwise {{ input.x }} reaches the reducer literally
// (the bug the first real docker run hit: item4.json carried "cveId":"{{ input.cve_id }}").
func TestRunCommandReduceTemplatesRun(t *testing.T) {
	rig := newReduceRig(t)
	rig.fake.ProgramExecAny(container.ExecResult{ExitCode: 0}, nil)
	wf := &ir.Workflow{
		ID: "x", Version: 1,
		Containers: map[string]ir.Container{
			reduceContainer: {Image: "oci://example.com/r@sha256:" + strings.Repeat("0", 64)},
		},
		Input: &ir.JSONSchema{
			"type":       "object",
			"properties": map[string]any{"cve_id": map[string]any{"type": "string"}},
		},
		Graph: ir.NodeList{},
	}
	rs := NewRunState(testRunID, testDigest, map[string]any{"cve_id": "CVE-2025-0001"})
	r := &ir.Reduce{Run: "echo {{ input.cve_id }}", Container: reduceContainer}
	branches := []reduceBranch{{N: 0, Outputs: map[string]any{"k": "a"}}}

	oc, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("runReduce: (%q, %v), want (ok, nil)", oc, err)
	}
	sawSubstituted := false
	for _, c := range rig.fake.Calls {
		if c.Run == "echo {{ input.cve_id }}" {
			t.Errorf("reduce run executed LITERALLY (not templated): %q", c.Run)
		}
		if c.Run == "echo CVE-2025-0001" {
			sawSubstituted = true
		}
	}
	if !sawSubstituted {
		t.Errorf("reduce run not templated to 'echo CVE-2025-0001'; Calls=%+v", rig.fake.Calls)
	}
}
