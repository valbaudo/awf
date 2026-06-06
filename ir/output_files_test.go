package ir

import (
	"encoding/json"
	"testing"
)

// list form: a bare []string unmarshals to unnamed entries; Paths() echoes them
// in declaration order.
func TestOutputFilesListUnmarshal(t *testing.T) {
	var o OutputFiles
	if err := json.Unmarshal([]byte(`["/out/a","/out/b"]`), &o); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(o) != 2 {
		t.Fatalf("len = %d, want 2", len(o))
	}
	if o[0] != (OutputFile{Path: "/out/a"}) || o[1] != (OutputFile{Path: "/out/b"}) {
		t.Fatalf("entries = %+v, want unnamed {/out/a},{/out/b}", o)
	}
	if got := o.Paths(); len(got) != 2 || got[0] != "/out/a" || got[1] != "/out/b" {
		t.Fatalf("Paths() = %v, want [/out/a /out/b]", got)
	}
	// no named entry → PathForName is always false
	if _, ok := o.PathForName("a"); ok {
		t.Fatalf("PathForName on unnamed entries returned ok=true")
	}
}

// map form: name→path unmarshals to entries sorted by name; PathForName resolves;
// Paths() returns the in-container paths (still by sorted-name order).
func TestOutputFilesMapUnmarshal(t *testing.T) {
	var o OutputFiles
	if err := json.Unmarshal([]byte(`{"report":"/out/r.md","poc":"/out/p.py"}`), &o); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	if len(o) != 2 {
		t.Fatalf("len = %d, want 2", len(o))
	}
	// sorted by name: poc < report
	if o[0] != (OutputFile{Name: "poc", Path: "/out/p.py"}) ||
		o[1] != (OutputFile{Name: "report", Path: "/out/r.md"}) {
		t.Fatalf("entries = %+v, want sorted [poc report]", o)
	}
	if p, ok := o.PathForName("report"); !ok || p != "/out/r.md" {
		t.Fatalf("PathForName(report) = %q,%v, want /out/r.md,true", p, ok)
	}
	if p, ok := o.PathForName("poc"); !ok || p != "/out/p.py" {
		t.Fatalf("PathForName(poc) = %q,%v, want /out/p.py,true", p, ok)
	}
	if _, ok := o.PathForName("missing"); ok {
		t.Fatalf("PathForName(missing) returned ok=true")
	}
	if got := o.Paths(); len(got) != 2 || got[0] != "/out/p.py" || got[1] != "/out/r.md" {
		t.Fatalf("Paths() = %v, want [/out/p.py /out/r.md]", got)
	}
}

// marshal is shape-preserving: bare list re-emits []string (digest-stable);
// named map re-emits a name→path object.
func TestOutputFilesMarshalShape(t *testing.T) {
	bare := OutputFiles{{Path: "/out/a"}, {Path: "/out/b"}}
	b, err := json.Marshal(bare)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `["/out/a","/out/b"]` {
		t.Fatalf("bare marshal = %s, want a JSON list", b)
	}

	named := OutputFiles{{Name: "poc", Path: "/out/p.py"}, {Name: "report", Path: "/out/r.md"}}
	b, err = json.Marshal(named)
	if err != nil {
		t.Fatal(err)
	}
	// json.Marshal of a map sorts keys; we can assert the exact bytes.
	if string(b) != `{"poc":"/out/p.py","report":"/out/r.md"}` {
		t.Fatalf("named marshal = %s, want a name→path object", b)
	}
}

// a value that is neither a list nor a map is rejected.
func TestOutputFilesGarbageRejected(t *testing.T) {
	var o OutputFiles
	if err := json.Unmarshal([]byte(`42`), &o); err == nil {
		t.Fatalf("unmarshal of 42 should fail")
	}
	if err := json.Unmarshal([]byte(`{"report":7}`), &o); err == nil {
		t.Fatalf("unmarshal of a non-string map value should fail")
	}
}

// input_files unmarshals as a map of in-container destination path → a static
// artifact reference (step.<id>.files.<name>), on both code and agent steps.
func TestInputFilesUnmarshal(t *testing.T) {
	var cs CodeStep
	if err := json.Unmarshal([]byte(`{"id":"hunt","run":"x","input_files":{"/work/report.md":"step.recon.files.report"}}`), &cs); err != nil {
		t.Fatalf("unmarshal code step: %v", err)
	}
	if got := cs.InputFiles["/work/report.md"]; got != "step.recon.files.report" {
		t.Fatalf("code InputFiles[/work/report.md] = %q, want step.recon.files.report", got)
	}

	var as AgentStep
	if err := json.Unmarshal([]byte(`{"id":"hunt","uses":"anthropic/claude","input_files":{"/work/report.md":"step.recon.files.report"}}`), &as); err != nil {
		t.Fatalf("unmarshal agent step: %v", err)
	}
	if got := as.InputFiles["/work/report.md"]; got != "step.recon.files.report" {
		t.Fatalf("agent InputFiles[/work/report.md] = %q, want step.recon.files.report", got)
	}
}

// OutputFilesByStepID indexes every code/agent step's output_files in one walk.
func TestOutputFilesByStepID(t *testing.T) {
	wf := &Workflow{
		Graph: NodeList{
			&CodeStep{ID: "recon", Run: "x", OutputFiles: OutputFiles{{Name: "report", Path: "/out/r.md"}}},
			&AgentStep{ID: "hunt", Uses: "anthropic/claude", OutputFiles: OutputFiles{{Name: "findings", Path: "/out/f.md"}}},
			&SignalStep{ID: "wait", Await: "x"},
		},
	}
	idx := OutputFilesByStepID(wf)
	if len(idx) != 2 {
		t.Fatalf("index len = %d, want 2 (signal step excluded)", len(idx))
	}
	if p, ok := idx["recon"].PathForName("report"); !ok || p != "/out/r.md" {
		t.Fatalf("recon report = %q,%v, want /out/r.md,true", p, ok)
	}
	if p, ok := idx["hunt"].PathForName("findings"); !ok || p != "/out/f.md" {
		t.Fatalf("hunt findings = %q,%v, want /out/f.md,true", p, ok)
	}
}
