package engine

import (
	"context"
	"encoding/json"
	"reflect"
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
	r := &ir.Reduce{Quorum: &q, Field: "vulnerable"}

	oc, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), minimalReduceWorkflow(), RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
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
	r := &ir.Reduce{Quorum: &q, Field: "vulnerable"}

	if _, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), minimalReduceWorkflow(), RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{}); err != nil {
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
	r := &ir.Reduce{Quorum: &q, Field: "vulnerable"}

	oc, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), minimalReduceWorkflow(), RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
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
	r := &ir.Reduce{Quorum: &q, Field: "vulnerable"}

	oc, err := runReduce(context.Background(), r, testMapPath, branches, 3 /* cohort */, minimalReduceWorkflow(), RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
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
	r := &ir.Reduce{Quorum: &q, Field: "vulnerable"}

	oc, err := runReduce(context.Background(), r, testMapPath, nil /* all crashed */, 3 /* cohort */, minimalReduceWorkflow(), RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
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
	r := &ir.Reduce{Quorum: &q, Field: "vulnerable"}

	oc, err := runReduce(context.Background(), r, testMapPath, nil, 0, minimalReduceWorkflow(), RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
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
		{N: 0, Outputs: map[string]any{"k": "a"}, Files: map[string]string{"row": row0}},
		{N: 1, Outputs: map[string]any{"k": "b"}, Files: map[string]string{"row": row1}},
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

	oc, err := runReduce(context.Background(), r, mapPath, branches, len(branches), minimalReduceWorkflow(), RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
	if err != nil {
		t.Fatalf("runReduce (run): %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want ok", oc)
	}

	// The reducer's container received the manifest + each branch's artifact.
	// The Fake backend returns StagingRoot: "/work/.awf" (docker-equivalent).
	h := rig.ld.Handles[reduceContainer]
	captured, cerr := rig.fake.CaptureFiles(context.Background(), h, []string{
		"/work/.awf/aggregate.json",
		"/work/.awf/branch-0/row",
		"/work/.awf/branch-1/row",
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
			Concurrency: intPtr(1),
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
	oc, err := runReduce(context.Background(), r, mapPath, branches, len(branches), wf, RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
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
		InputSchema: &ir.JSONSchema{
			"type":       "object",
			"properties": map[string]any{"cve_id": map[string]any{"type": "string"}},
		},
		Graph: ir.NodeList{},
	}
	rs := NewRunState(testRunID, testDigest, map[string]any{"cve_id": "CVE-2025-0001"})
	r := &ir.Reduce{Run: "echo {{ input.cve_id }}", Container: reduceContainer}
	branches := []reduceBranch{{N: 0, Outputs: map[string]any{"k": "a"}}}

	oc, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), wf, RootModuleID, rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
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

// ---------------------------------------------------------------------------
// WS-5: per-backend staging root + AWF_STAGING_ROOT env injection
// ---------------------------------------------------------------------------

// stagingRootBackend wraps container.Fake and overrides Capabilities to return
// a given StagingRoot — letting us simulate native (StagingRoot: ".awf") vs.
// docker (StagingRoot: "/work/.awf") without touching the Fake itself.
type stagingRootBackend struct {
	*container.Fake
	stagingRoot string
}

func (b *stagingRootBackend) Capabilities() container.Caps {
	caps := b.Fake.Capabilities()
	caps.StagingRoot = b.stagingRoot
	return caps
}

// newReduceRigWithStagingRoot builds a rig whose backend reports the given
// StagingRoot, letting us assert native-vs-docker staging paths.
func newReduceRigWithStagingRoot(t *testing.T, stagingRoot string) *reduceRig {
	t.Helper()
	fake := container.NewFake()
	h, err := fake.Create(context.Background(), container.ContainerSpec{Name: reduceContainer})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	wrapped := &stagingRootBackend{Fake: fake, stagingRoot: stagingRoot}
	clk := &clock.Fake{T: testClockEpoch}
	return &reduceRig{
		ld:    &LocalDispatcher{Backend: wrapped, Handles: map[string]container.Handle{reduceContainer: h}},
		fake:  fake,
		clk:   clk,
		lg:    state.NewInMemoryLog(clk),
		blobs: state.NewInMemoryBlobs(),
		rs:    NewRunState(testRunID, testDigest, nil),
	}
}

// TestRunReduceNativeStagingRoot asserts that on a backend with StagingRoot:
// ".awf" (native), the reducer's manifest lands at ".awf/aggregate.json" (NOT
// "/work/.awf/aggregate.json") and AWF_STAGING_ROOT is in the reducer env.
// This is the RED test — it fails before WS-5 is implemented.
func TestRunReduceNativeStagingRoot(t *testing.T) {
	rig := newReduceRigWithStagingRoot(t, ".awf")
	mapPath := testMapPath

	ref0, err := rig.blobs.Put([]byte("data-0"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	branches := []reduceBranch{
		{N: 0, Outputs: map[string]any{"k": "v"}, Files: map[string]string{"f": ref0}},
	}

	rig.fake.ProgramExec("./merge.sh", container.ExecResult{ExitCode: 0}, nil)

	r := &ir.Reduce{Run: "./merge.sh", Container: reduceContainer}
	_, err = runReduce(context.Background(), r, mapPath, branches, len(branches), minimalReduceWorkflow(), RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
	if err != nil {
		t.Fatalf("runReduce: %v", err)
	}

	// Assert manifest staged under ".awf/aggregate.json" (not "/work/.awf/...").
	h := rig.ld.Handles[reduceContainer]
	if _, cerr := rig.fake.CaptureFiles(context.Background(), h, []string{".awf/aggregate.json"}); cerr != nil {
		t.Errorf("native staging: manifest NOT staged at .awf/aggregate.json: %v", cerr)
	}
	if _, cerr := rig.fake.CaptureFiles(context.Background(), h, []string{"/work/.awf/aggregate.json"}); cerr == nil {
		t.Errorf("native staging: manifest ALSO staged at /work/.awf/aggregate.json (should only be at .awf/)")
	}

	// Assert AWF_STAGING_ROOT is in the reducer step env.
	gotRoot := ""
	for _, c := range rig.fake.Calls {
		if c.Run == "./merge.sh" {
			gotRoot = c.Env["AWF_STAGING_ROOT"]
		}
	}
	if gotRoot != ".awf" {
		t.Errorf("AWF_STAGING_ROOT = %q, want %q", gotRoot, ".awf")
	}
}

// TestRunReduceDockerStagingRoot asserts that on a backend with StagingRoot:
// "/work/.awf" (docker), the reducer's manifest lands at "/work/.awf/aggregate.json"
// and AWF_STAGING_ROOT is "/work/.awf". Docker behavior must remain byte-identical.
func TestRunReduceDockerStagingRoot(t *testing.T) {
	// Docker rig uses the normal fake (which will return "/work/.awf" after WS-5).
	rig := newReduceRig(t)
	mapPath := testMapPath

	branches := []reduceBranch{
		{N: 0, Outputs: map[string]any{"k": "v"}, Files: map[string]string{}},
	}

	rig.fake.ProgramExec("./merge.sh", container.ExecResult{ExitCode: 0}, nil)

	r := &ir.Reduce{Run: "./merge.sh", Container: reduceContainer}
	_, err := runReduce(context.Background(), r, mapPath, branches, len(branches), minimalReduceWorkflow(), RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
	if err != nil {
		t.Fatalf("runReduce: %v", err)
	}

	// Assert manifest staged at "/work/.awf/aggregate.json".
	h := rig.ld.Handles[reduceContainer]
	if _, cerr := rig.fake.CaptureFiles(context.Background(), h, []string{"/work/.awf/aggregate.json"}); cerr != nil {
		t.Errorf("docker staging: manifest NOT staged at /work/.awf/aggregate.json: %v", cerr)
	}

	// Assert AWF_STAGING_ROOT is "/work/.awf".
	gotRoot := ""
	for _, c := range rig.fake.Calls {
		if c.Run == "./merge.sh" {
			gotRoot = c.Env["AWF_STAGING_ROOT"]
		}
	}
	if gotRoot != "/work/.awf" {
		t.Errorf("AWF_STAGING_ROOT = %q, want %q", gotRoot, "/work/.awf")
	}
}

func TestRunReduceCommandDefaultsToOneAttempt(t *testing.T) {
	rig := newReduceRig(t)
	rig.fake.ProgramExec("./merge.sh", container.ExecResult{ExitCode: 0}, nil)
	rig.fake.FailExecAfterN(0)
	r := &ir.Reduce{Run: "./merge.sh", Container: reduceContainer}
	branches := []reduceBranch{{N: 0, Outputs: map[string]any{"k": "v"}}}

	oc, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), minimalReduceWorkflow(), RootModuleID, rig.rs, nil, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
	if oc != OutcomeRetryableFailure || err == nil {
		t.Fatalf("runReduce = (%q, %v), want retryable failure", oc, err)
	}
	if got := len(rig.fake.Calls); got != 1 {
		t.Errorf("reducer dispatches = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// I1: env: forwarding into the reduce.run reducer (mirrors F15 for graph run:
// steps) — a resolved workflow env: name reaches the reducer's Exec env on
// EVERY backend, and the engine-injected AWF_STAGING_ROOT key wins a name
// collision (the reducer is a `run:`-shaped step exactly like a graph code
// step; F15 wired the graph step, not this one).
// ---------------------------------------------------------------------------

// TestRunReduceCommandForwardsWorkflowEnv is the RED test for the missing
// half of I1: before the fix, runCommandReduce hardcoded
// Env: map[string]string{"AWF_STAGING_ROOT": stagingRoot}, dropping any
// forwarded workflow env: value.
func TestRunReduceCommandForwardsWorkflowEnv(t *testing.T) {
	rig := newReduceRig(t)
	rig.fake.ProgramExec("./merge.sh", container.ExecResult{ExitCode: 0}, nil)

	r := &ir.Reduce{Run: "./merge.sh", Container: reduceContainer}
	branches := []reduceBranch{{N: 0, Outputs: map[string]any{"k": "v"}}}
	runEnv := map[string]string{"MY_VAR": "hello"}

	oc, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), minimalReduceWorkflow(), RootModuleID, rig.rs, runEnv, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{})
	if err != nil {
		t.Fatalf("runReduce: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want ok", oc)
	}

	got := map[string]string(nil)
	for _, c := range rig.fake.Calls {
		if c.Run == "./merge.sh" {
			got = c.Env
		}
	}
	if got["MY_VAR"] != "hello" {
		t.Errorf("reducer Env[MY_VAR] = %q, want %q (forwarded workflow env:)", got["MY_VAR"], "hello")
	}
	if got["AWF_STAGING_ROOT"] != "/work/.awf" {
		t.Errorf("reducer Env[AWF_STAGING_ROOT] = %q, want %q", got["AWF_STAGING_ROOT"], "/work/.awf")
	}
	// Fresh-copy invariant (F15): the caller's runEnv map must never be mutated.
	if _, leaked := runEnv["AWF_STAGING_ROOT"]; leaked {
		t.Errorf("runEnv mutated in place: AWF_STAGING_ROOT leaked into the caller's map %v", runEnv)
	}
}

// TestRunReduceCommandEngineEnvWinsCollision asserts the engine-injected
// AWF_STAGING_ROOT wins over a workflow env: declaration of the same name —
// the author cannot override the engine's staging contract via env:.
func TestRunReduceCommandEngineEnvWinsCollision(t *testing.T) {
	rig := newReduceRig(t)
	rig.fake.ProgramExec("./merge.sh", container.ExecResult{ExitCode: 0}, nil)

	r := &ir.Reduce{Run: "./merge.sh", Container: reduceContainer}
	branches := []reduceBranch{{N: 0, Outputs: map[string]any{"k": "v"}}}
	runEnv := map[string]string{"AWF_STAGING_ROOT": "author-supplied-value"}

	if _, err := runReduce(context.Background(), r, testMapPath, branches, len(branches), minimalReduceWorkflow(), RootModuleID, rig.rs, runEnv, rig.ld, rig.lg, rig.blobs, rig.clk, nil, reduceCallContext{}); err != nil {
		t.Fatalf("runReduce: %v", err)
	}

	got := ""
	for _, c := range rig.fake.Calls {
		if c.Run == "./merge.sh" {
			got = c.Env["AWF_STAGING_ROOT"]
		}
	}
	if got != "/work/.awf" {
		t.Errorf("AWF_STAGING_ROOT = %q, want %q (engine key must win a name collision with env:)", got, "/work/.awf")
	}
}

// TestCollectReduceBranchesForwardsGateTypedOutputs pins that a map body whose
// producer lives inside a gate contributes its ACCEPTED attempt's typed outputs
// to the reduce fan-in, not just its files. Before this change gateForwarded
// suppressed them and each branch's Outputs came back empty.
func TestCollectReduceBranchesForwardsGateTypedOutputs(t *testing.T) {
	mapPath := "map[0]"
	rs := &RunState{
		RunID: testRunID,
		MapItems: map[string][]MapItemRecord{
			mapPath: {
				{N: 0, Status: ItemPassed},
				{N: 1, Status: ItemPassed},
			},
		},
		GateAttempts: map[string][]AttemptResult{
			"map[0].item-0.gate[0]": {
				{N: 1, AttemptOutcome: AttemptRejected},
				{N: 2, AttemptOutcome: AttemptPassed},
			},
			"map[0].item-1.gate[0]": {
				{N: 1, AttemptOutcome: AttemptPassed},
			},
		},
		Completed: map[string]NodeResult{
			"map[0].item-0.gate[0].attempt-1.generate.draft": {Outcome: OutcomeOK, Outputs: map[string]any{"score": 1.0}},
			"map[0].item-0.gate[0].attempt-2.generate.draft": {Outcome: OutcomeOK, Outputs: map[string]any{"score": 2.0}},
			"map[0].item-1.gate[0].attempt-1.generate.draft": {Outcome: OutcomeOK, Outputs: map[string]any{"score": 7.0}},
		},
	}
	mapNode := &ir.Map{
		Over: ir.Expr("{{ input.items }}"),
		As:   "item",
		Body: ir.NodeList{
			&ir.Gate{
				Generate: ir.NodeList{
					&ir.CodeStep{ID: "draft", Run: "gen", Container: "c0"},
				},
				Evaluate: ir.NodeList{
					&ir.CodeStep{ID: "judge", Run: "eval", Container: "c0"},
				},
				Until:       ir.Expr("{{ evaluate.ok }}"),
				MaxAttempts: 3,
			},
		},
	}
	wf := &ir.Workflow{ID: "mapgate", Version: 1, Graph: ir.NodeList{mapNode}}

	// collectReduceBranches derives its own producer list by walking mapNode.Body
	// with ir.WalkNodes(n.Body, "body", ...) and stripping the "body." prefix, so
	// the gate's generate step yields suffix "gate[0].generate.draft" — which is
	// what itemBodyStepPath splices the accepted attempt into.
	branches, err := collectReduceBranches(rs, mapNode, mapPath, wf)
	if err != nil {
		t.Fatalf("collectReduceBranches: %v", err)
	}
	if len(branches) != 2 {
		t.Fatalf("branches = %d, want 2", len(branches))
	}
	// item-0 must carry ATTEMPT 2's score (the accepted attempt), not attempt 1's.
	if got := branches[0].Outputs["score"]; got != 2.0 {
		t.Errorf("branch 0 score = %v, want 2.0 (accepted attempt-2, not rejected attempt-1)", got)
	}
	if got := branches[1].Outputs["score"]; got != 7.0 {
		t.Errorf("branch 1 score = %v, want 7.0", got)
	}
}

// TestCollectReduceBranchesForwardsGateTypedOutputsAcrossFold closes the gap
// TestCollectReduceBranchesForwardsGateTypedOutputs left open: that test hand-builds
// a RunState resembling folded state but never calls engine.Fold, so conformance row
// (e) — "resume across a committed gate resolves the identical value" — was proven
// for the sequential scalar path (TestGateForwardingIsFoldStableAcrossResume in
// gate_test.go) but not the reduce fan-in.
//
// This drives a REAL map -> gate through runMap: two items ("a", "b") over a body
// whose sole node is a Gate. Item "a" is rejected on attempt 1 and passes on attempt
// 2 with a DIFFERENT typed score, so a fold that (incorrectly) picked the first
// attempt instead of the accepted one would be caught. Item "b" passes on attempt 1.
// The generate step's Run threads {{ evaluate.feedback }} and the evaluate step's Run
// threads {{ step.draft.score }} so each attempt dispatches a distinct command
// against the fake backend (Fake.Exec is keyed on Cmd.Run) — real gate.attempt /
// map.item / node.completed events land in the journal exactly as a live run
// produces them, not a hand-built approximation.
//
// It then rebuilds RunState purely by folding that journal (lg.Fold() ->
// engine.Fold(events, blobs), the resume path) and asserts collectReduceBranches
// against the FOLDED RunState returns branches deeply equal to the ones collected
// against the LIVE RunState — and that both carry the accepted attempt's score, not
// the first attempt's.
func TestCollectReduceBranchesForwardsGateTypedOutputsAcrossFold(t *testing.T) {
	verdict := func(verified bool, feedback string) []byte {
		raw, err := json.Marshal(map[string]any{"verified": verified, "feedback": feedback})
		if err != nil {
			t.Fatalf("marshal verdict: %v", err)
		}
		return raw
	}
	gateBody := ir.NodeList{
		&ir.Gate{
			Generate: ir.NodeList{
				&ir.CodeStep{ID: "draft", Run: "./draft {{ x }} {{ evaluate.feedback }}", Container: testMapContainer,
					OutputSchema: scoreSchema, Retry: &ir.RetryPolicy{Attempts: 1}},
			},
			Evaluate: ir.NodeList{
				&ir.CodeStep{ID: "judge", Run: "./judge {{ x }} {{ step.draft.score }}", Container: testMapContainer,
					OutputSchema: schemaForVerdict(), Retry: &ir.RetryPolicy{Attempts: 1}},
			},
			Until:       ir.Expr("{{ evaluate.verified }}"),
			MaxAttempts: 3,
		},
	}
	wf := staticOverWorkflow("x", gateBody, 1, nil)
	mapNode := wf.Graph[0].(*ir.Map)

	rig := newMapRig(t,
		// item "a": attempt 1 rejected (score 1, feedback "more"), attempt 2
		// accepted (score 2) — the accepted attempt's score must be the one
		// that survives into the branch, both live and folded.
		execProgram{cmd: "./draft a ", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"score":1}`)}},
		execProgram{cmd: "./judge a 1", res: container.ExecResult{ExitCode: 0, AWFOutput: verdict(false, "more")}},
		execProgram{cmd: "./draft a more", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"score":2}`)}},
		execProgram{cmd: "./judge a 2", res: container.ExecResult{ExitCode: 0, AWFOutput: verdict(true, "good")}},
		// item "b": passes on attempt 1 (score 7).
		execProgram{cmd: "./draft b ", res: container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"score":7}`)}},
		execProgram{cmd: "./judge b 7", res: container.ExecResult{ExitCode: 0, AWFOutput: verdict(true, "fine")}},
	)
	input := runOverItems("a", "b")
	// Seed run.started FIRST so the journal's first event is run.started (Fold
	// requires it) — runMap below appends map.item / gate.attempt / node.completed
	// events onto the SAME log, so the eventual Fold sees the whole genuine run.
	seedRunStartedWithInput(t, rig.lg, rig.blobs, input)

	rs := NewRunState(testRunID, testDigest, input)
	oc, err := runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)
	if oc != OutcomeOK || err != nil {
		t.Fatalf("runMap: got (%q, %v), want (ok, nil)", oc, err)
	}

	liveBranches, err := collectReduceBranches(rs, mapNode, testMapPath, wf)
	if err != nil {
		t.Fatalf("collectReduceBranches (live): %v", err)
	}
	if len(liveBranches) != 2 {
		t.Fatalf("live branches = %d, want 2", len(liveBranches))
	}
	if got := liveBranches[0].Outputs["score"]; got != 2.0 {
		t.Errorf("live branch 0 score = %v, want 2.0 (accepted attempt-2, not rejected attempt-1)", got)
	}
	if got := liveBranches[1].Outputs["score"]; got != 7.0 {
		t.Errorf("live branch 1 score = %v, want 7.0", got)
	}

	// Rebuild RunState purely by folding the journal — the resume path.
	events, ferr := rig.lg.Fold()
	if ferr != nil {
		t.Fatalf("lg.Fold: %v", ferr)
	}
	rs2, foldErr := Fold(events, rig.blobs)
	if foldErr != nil {
		t.Fatalf("engine.Fold: %v", foldErr)
	}
	foldedBranches, err := collectReduceBranches(rs2, mapNode, testMapPath, wf)
	if err != nil {
		t.Fatalf("collectReduceBranches (folded): %v", err)
	}

	if !reflect.DeepEqual(liveBranches, foldedBranches) {
		t.Errorf("folded branches = %+v, want identical to live branches %+v — reduce fan-in forwarding must be fold-stable", foldedBranches, liveBranches)
	}
	if got := foldedBranches[0].Outputs["score"]; got != 2.0 {
		t.Errorf("folded branch 0 score = %v, want 2.0 (accepted attempt-2, not rejected attempt-1)", got)
	}
	if got := foldedBranches[1].Outputs["score"]; got != 7.0 {
		t.Errorf("folded branch 1 score = %v, want 7.0", got)
	}
}
