package conformance

import (
	"reflect"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

func testSubworkflow(t *testing.T, factory BackendFactory) {
	t.Helper()
	if _, ok := factory().(*container.Fake); !ok {
		t.Skip("subworkflow bucket inspects fake backend calls; fake-only")
	}
	t.Run("simple_call_exports_typed_output", func(t *testing.T) {
		testSubworkflowSimpleCallExportsTypedOutput(t, factory)
	})
	t.Run("half_commit_resume_commits_call_boundary_once", func(t *testing.T) {
		testSubworkflowHalfCommitResume(t, factory)
	})
	t.Run("artifact_export_stages_into_parent", func(t *testing.T) {
		testSubworkflowArtifactExport(t, factory)
	})
	t.Run("named_aggregate_artifact_export", func(t *testing.T) {
		testSubworkflowNamedAggregateArtifactExport(t, factory)
	})
	t.Run("module_asset_collision_resume_uses_run_started_bytes", func(t *testing.T) {
		testSubworkflowModuleAssetCollisionResume(t, factory)
	})
	t.Run("repeated_call_isolation", func(t *testing.T) {
		testSubworkflowRepeatedCallIsolation(t, factory)
	})
	t.Run("nested_call_path", func(t *testing.T) {
		testSubworkflowNestedCallPath(t, factory)
	})
	t.Run("digest_drift", func(t *testing.T) {
		testSubworkflowDigestDrift(t, factory)
	})
}

func testSubworkflowSimpleCallExportsTypedOutput(t *testing.T, factory BackendFactory) {
	t.Helper()

	var fake *container.Fake
	h := newHarness(t, func() container.Backend {
		f := container.NewFake()
		f.ProgramExec("./child.sh alpha", container.ExecResult{
			ExitCode:  0,
			AWFOutput: []byte(`{"summary":"child saw alpha"}`),
		}, nil)
		f.ProgramExec("./parent.sh child saw alpha", container.ExecResult{ExitCode: 0}, nil)
		fake = f
		return f
	}, subworkflowSimpleRootWorkflow)
	writeSubworkflowFile(t, h, "child.awf.yaml", subworkflowSimpleChildWorkflow)

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}

	rs, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	call, ok := rs.LookupCompleted("child_call")
	if !ok {
		t.Fatal("missing call boundary completion at child_call")
	}
	if got := call.Outputs["summary"]; got != "child saw alpha" {
		t.Fatalf("child_call summary = %v, want child saw alpha", got)
	}
	if _, ok := rs.LookupCompleted("child_call.workflow.final"); !ok {
		t.Fatal("missing child leaf completion at child_call.workflow.final")
	}
	if fake == nil {
		t.Fatal("fake was not created")
	}
	if !sawExec(fake, "./parent.sh child saw alpha") {
		t.Fatalf("parent did not consume step.child_call.summary; calls = %+v", fake.Calls)
	}
}

func testSubworkflowHalfCommitResume(t *testing.T, factory BackendFactory) {
	t.Helper()

	var resumeFake *container.Fake
	h := newHarness(t, func() container.Backend {
		f := factory().(*container.Fake)
		f.ProgramExec("./parent.sh from seeded child", container.ExecResult{ExitCode: 0}, nil)
		resumeFake = f
		return f
	}, subworkflowSimpleRootWorkflow)
	writeSubworkflowFile(t, h, "child.awf.yaml", subworkflowSimpleChildWorkflow)
	seedSubworkflowHalfCommit(t, h, "child_call", map[string]any{"topic": "alpha"}, map[string]any{"summary": "from seeded child"})

	oc, err := h.resumeWorkflow(t)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("resume outcome = %q, want ok", oc)
	}
	if sawExec(resumeFake, "./child.sh alpha") {
		t.Fatalf("resume dispatched committed child leaf again; calls = %+v", resumeFake.Calls)
	}
	if got := countNodeCompleted(mustFoldEvents(t, h), "child_call"); got != 1 {
		t.Fatalf("call boundary node.completed count = %d, want exactly 1", got)
	}

	rs, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("Fold after resume: %v", err)
	}
	call, ok := rs.LookupCompleted("child_call")
	if !ok {
		t.Fatal("missing call boundary completion after resume")
	}
	if got := call.Outputs["summary"]; got != "from seeded child" {
		t.Fatalf("folded call summary = %v, want from seeded child", got)
	}
}

func testSubworkflowArtifactExport(t *testing.T, factory BackendFactory) {
	t.Helper()

	report := []byte("child report\n")
	var spy *assetCopyToSpy
	h := newHarness(t, func() container.Backend {
		f := factory().(*container.Fake)
		f.ProgramExecWithFiles("./make-report.sh", container.ExecResult{ExitCode: 0}, nil,
			map[string][]byte{"/out/report.md": report})
		f.ProgramExec("./consume-report.sh", container.ExecResult{ExitCode: 0}, nil)
		spy = newAssetCopyToSpy(f)
		return spy
	}, subworkflowArtifactRootWorkflow)
	writeSubworkflowFile(t, h, "child.awf.yaml", subworkflowArtifactChildWorkflow)

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}
	staged := spy.stagedByPath()
	if staged["/work/report.md"] != string(report) {
		t.Fatalf("staged /work/report.md = %q, want %q (all staged: %#v)", staged["/work/report.md"], report, staged)
	}
	rs, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	call, ok := rs.LookupCompleted("child_call")
	if !ok {
		t.Fatal("missing child_call completion")
	}
	ref, ok := call.Files["report"]
	if !ok {
		t.Fatalf("call.Files missing exported report: %#v", call.Files)
	}
	got, err := h.blobs.Get(ref)
	if err != nil {
		t.Fatalf("Blobs.Get(%q): %v", ref, err)
	}
	if string(got) != string(report) {
		t.Fatalf("exported report blob = %q, want %q", got, report)
	}
}

func testSubworkflowNamedAggregateArtifactExport(t *testing.T, factory BackendFactory) {
	t.Helper()

	merged := []byte("a-row\nb-row\n")
	var spy *assetCopyToSpy
	h := newHarness(t, func() container.Backend {
		f := factory().(*container.Fake)
		f.ProgramExecWithFiles("./row.sh a", container.ExecResult{ExitCode: 0}, nil,
			map[string][]byte{"/out/leaf.csv": []byte("a-row\n")})
		f.ProgramExecWithFiles("./row.sh b", container.ExecResult{ExitCode: 0}, nil,
			map[string][]byte{"/out/leaf.csv": []byte("b-row\n")})
		f.ProgramExecWithFiles("./merge.sh", container.ExecResult{
			ExitCode:  0,
			AWFOutput: []byte(`{"csv_rows":2}`),
		}, nil, map[string][]byte{"/out/versions.csv": merged})
		f.ProgramExec("./consume-aggregate.sh", container.ExecResult{ExitCode: 0}, nil)
		spy = newAssetCopyToSpy(f)
		return spy
	}, subworkflowAggregateRootWorkflow)
	writeSubworkflowFile(t, h, "child.awf.yaml", subworkflowAggregateChildWorkflow)

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}
	staged := spy.stagedByPath()
	if staged["/work/versions.csv"] != string(merged) {
		t.Fatalf("staged /work/versions.csv = %q, want %q (all staged: %#v)", staged["/work/versions.csv"], merged, staged)
	}
	rs, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if _, ok := rs.LookupCompleted("child_call.workflow.map[0]"); !ok {
		t.Fatal("missing child reduced map completion at child_call.workflow.map[0]")
	}
	call, ok := rs.LookupCompleted("child_call")
	if !ok {
		t.Fatal("missing child_call completion")
	}
	ref, ok := call.Files["item4"]
	if !ok {
		t.Fatalf("call.Files missing exported item4: %#v", call.Files)
	}
	got, err := h.blobs.Get(ref)
	if err != nil {
		t.Fatalf("Blobs.Get(%q): %v", ref, err)
	}
	if string(got) != string(merged) {
		t.Fatalf("exported aggregate blob = %q, want %q", got, merged)
	}
}

func testSubworkflowModuleAssetCollisionResume(t *testing.T, factory BackendFactory) {
	t.Helper()

	var runSpy, resumeSpy *assetCopyToSpy
	h := newHarness(t, func() container.Backend {
		f := factory().(*container.Fake)
		f.ProgramExec("./consume-child-schema.sh", container.ExecResult{ExitCode: 0}, nil)
		f.ProgramExec("./consume-root-schema.sh", container.ExecResult{ExitCode: 0}, nil)
		spy := newAssetCopyToSpy(f)
		if runSpy == nil {
			f.FailExecAfterN(0)
			runSpy = spy
		} else {
			resumeSpy = spy
		}
		return spy
	}, subworkflowAssetCollisionRootWorkflow)
	writeSubworkflowFile(t, h, "child.awf.yaml", subworkflowAssetCollisionChildWorkflow)
	writeSubworkflowFile(t, h, "root/schema.json", "root schema v1\n")
	writeSubworkflowFile(t, h, "child/schema.json", "child schema v1\n")
	loadedBeforeMutation := loadSubworkflowDefinition(t, h)

	oc, _ := h.runWorkflow(t)
	if oc == "" {
		t.Fatal("first run produced no outcome")
	}
	if oc == engine.OutcomeOK {
		t.Fatal("first run unexpectedly ok; child step should crash after staging")
	}
	writeSubworkflowFile(t, h, "root/schema.json", "root schema v2\n")
	writeSubworkflowFile(t, h, "child/schema.json", "child schema v2\n")

	oc2, err := runLoadedSubworkflowResume(t, h, loadedBeforeMutation)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if oc2 != engine.OutcomeOK {
		t.Fatalf("resume outcome = %q, want ok", oc2)
	}
	if resumeSpy == nil {
		t.Fatal("resume spy was not created")
	}
	if got := runSpy.stagedByPath(); got["/work/child-schema.json"] != "child schema v1\n" {
		t.Fatalf("first run child schema staged = %q, want v1 (all staged: %#v)", got["/work/child-schema.json"], got)
	}
	want := map[string]string{
		"/work/child-schema.json": "child schema v1\n",
		"/work/root-schema.json":  "root schema v1\n",
	}
	if got := resumeSpy.stagedByPath(); !reflect.DeepEqual(got, want) {
		t.Fatalf("resume staged files = %#v, want %#v", got, want)
	}
}

func testSubworkflowRepeatedCallIsolation(t *testing.T, factory BackendFactory) {
	t.Helper()

	var fake *container.Fake
	h := newHarness(t, func() container.Backend {
		f := factory().(*container.Fake)
		f.ProgramExec("./child.sh one", container.ExecResult{
			ExitCode:  0,
			AWFOutput: []byte(`{"summary":"one-result"}`),
		}, nil)
		f.ProgramExec("./child.sh two", container.ExecResult{
			ExitCode:  0,
			AWFOutput: []byte(`{"summary":"two-result"}`),
		}, nil)
		f.ProgramExec("./combine.sh one-result two-result", container.ExecResult{ExitCode: 0}, nil)
		fake = f
		return f
	}, subworkflowRepeatedRootWorkflow)
	writeSubworkflowFile(t, h, "child.awf.yaml", subworkflowSimpleChildWorkflow)

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}
	rs, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	first, ok := rs.LookupCompleted("first_call")
	if !ok {
		t.Fatal("missing first_call completion")
	}
	second, ok := rs.LookupCompleted("second_call")
	if !ok {
		t.Fatal("missing second_call completion")
	}
	if first.Outputs["summary"] != "one-result" || second.Outputs["summary"] != "two-result" {
		t.Fatalf("call outputs = (%v, %v), want (one-result, two-result)", first.Outputs["summary"], second.Outputs["summary"])
	}
	childHandles := map[string]bool{}
	for i, c := range fake.Calls {
		if c.Run == "./child.sh one" || c.Run == "./child.sh two" {
			childHandles[fake.ExecHandles[i].ID] = true
		}
	}
	if len(childHandles) != 2 {
		t.Fatalf("child calls used %d container handles, want 2 isolated handles; handles=%v calls=%+v", len(childHandles), fake.ExecHandles, fake.Calls)
	}
}

func testSubworkflowNestedCallPath(t *testing.T, factory BackendFactory) {
	t.Helper()

	h := newHarness(t, func() container.Backend {
		f := factory().(*container.Fake)
		f.ProgramExec("./inner.sh", container.ExecResult{
			ExitCode:  0,
			AWFOutput: []byte(`{"result":"nested-ok"}`),
		}, nil)
		return f
	}, subworkflowNestedRootWorkflow)
	writeSubworkflowFile(t, h, "outer.awf.yaml", subworkflowNestedOuterWorkflow)
	writeSubworkflowFile(t, h, "inner.awf.yaml", subworkflowNestedInnerWorkflow)

	oc, err := h.runWorkflow(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if oc != engine.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", oc)
	}
	rs, err := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if _, ok := rs.LookupCompleted("outer_call.workflow.inner_call.workflow.final"); !ok {
		t.Fatalf("missing nested leaf completion; completed = %#v", rs.Completed)
	}
	if _, ok := rs.LookupCompleted("outer_call.workflow.inner_call"); !ok {
		t.Fatal("missing inner call boundary completion")
	}
	if _, ok := rs.LookupCompleted("outer_call"); !ok {
		t.Fatal("missing outer call boundary completion")
	}
}

func testSubworkflowDigestDrift(t *testing.T, factory BackendFactory) {
	t.Helper()

	t.Run("imported_workflow_semantics", func(t *testing.T) {
		h := newHarness(t, failingChildFactory(t, factory), subworkflowDriftRootWorkflow)
		writeSubworkflowFile(t, h, "child.awf.yaml", subworkflowDriftChildWorkflow("./child.sh"))

		oc, _ := h.runWorkflow(t)
		if oc == engine.OutcomeOK {
			t.Fatal("first run unexpectedly ok")
		}
		writeSubworkflowFile(t, h, "child.awf.yaml", subworkflowDriftChildWorkflow("./child-mutated.sh"))

		_, err := h.resumeWorkflow(t)
		if err == nil || !strings.Contains(err.Error(), "workflow digest mismatch") {
			t.Fatalf("resume err = %v, want workflow digest mismatch", err)
		}
	})

	t.Run("imported_asset_bytes", func(t *testing.T) {
		h := newHarness(t, failingChildFactory(t, factory), subworkflowAssetDriftRootWorkflow)
		writeSubworkflowFile(t, h, "child.awf.yaml", subworkflowAssetDriftChildWorkflow)
		writeSubworkflowFile(t, h, "child/schema.json", "schema v1\n")

		oc, _ := h.runWorkflow(t)
		if oc == engine.OutcomeOK {
			t.Fatal("first run unexpectedly ok")
		}
		writeSubworkflowFile(t, h, "child/schema.json", "schema v2\n")

		_, err := h.resumeWorkflow(t)
		if err == nil || !strings.Contains(err.Error(), "workflow digest mismatch") {
			t.Fatalf("resume err = %v, want workflow digest mismatch", err)
		}
	})

	t.Run("imported_comment_only_change", func(t *testing.T) {
		var resumeFake *container.Fake
		first := true
		h := newHarness(t, func() container.Backend {
			f := factory().(*container.Fake)
			f.ProgramExec("./child.sh", container.ExecResult{ExitCode: 0}, nil)
			if first {
				f.FailExecAfterN(0)
				first = false
			} else {
				resumeFake = f
			}
			return f
		}, subworkflowDriftRootWorkflow)
		writeSubworkflowFile(t, h, "child.awf.yaml", subworkflowDriftChildWorkflow("./child.sh"))

		oc, _ := h.runWorkflow(t)
		if oc == engine.OutcomeOK {
			t.Fatal("first run unexpectedly ok")
		}
		writeSubworkflowFile(t, h, "child.awf.yaml", "# harmless comment\n"+subworkflowDriftChildWorkflow("./child.sh"))

		oc2, err := h.resumeWorkflow(t)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if oc2 != engine.OutcomeOK {
			t.Fatalf("resume outcome = %q, want ok", oc2)
		}
		if resumeFake == nil || !sawExec(resumeFake, "./child.sh") {
			t.Fatalf("resume did not dispatch child after comment-only change; fake=%v", resumeFake)
		}
	})
}
