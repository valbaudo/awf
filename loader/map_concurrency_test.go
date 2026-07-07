package loader

import (
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/ir"
)

// mapWorkflowYAML builds a minimal single-map workflow. concurrencyLine is inserted
// verbatim into the map's field block (e.g. "concurrency: 3\n      " or "" to omit the
// key entirely) — F45 presence-tracking (*int) is exercised at both ends: omitted vs an
// explicit value.
func mapWorkflowYAML(id, concurrencyLine string) string {
	return "workflow: " + id + "\nversion: 1\n" +
		"containers:\n  c:\n    image: oci://x@sha256:" +
		"0000000000000000000000000000000000000000000000000000000000000000\n" +
		"graph:\n  - map:\n      over: \"{{ input.items }}\"\n      as: item\n      container: c\n      " +
		concurrencyLine +
		"body:\n        - id: a\n          container: c\n          run: \"true\"\n"
}

// firstMap extracts the sole top-level *ir.Map from a loaded root workflow's graph, or
// fails the test.
func firstMap(t *testing.T, wf *ir.Workflow) *ir.Map {
	t.Helper()
	if len(wf.Graph) != 1 {
		t.Fatalf("graph = %+v, want exactly 1 node", wf.Graph)
	}
	m, ok := wf.Graph[0].(*ir.Map)
	if !ok {
		t.Fatalf("graph[0] = %T, want *ir.Map", wf.Graph[0])
	}
	return m
}

// F45: an omitted `concurrency:` decodes to a nil *int, and loader.Load's desugar
// (applyMapConcurrencyDefault) sets it to a pointer to 1 (serial) BEFORE Load returns —
// i.e. before any digest/validate pass ever observes the IR.
func TestLoadDefaultsOmittedMapConcurrencyToOne(t *testing.T) {
	dir := t.TempDir()
	wfPath := writeWorkflow(t, dir, mapWorkflowYAML("root", ""))

	ld, err := Load(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	m := firstMap(t, ld.Workflow)
	if m.Concurrency == nil {
		t.Fatal("Concurrency = nil after Load, want defaulted to a non-nil pointer to 1")
	}
	if *m.Concurrency != 1 {
		t.Fatalf("Concurrency = %d, want 1 (default)", *m.Concurrency)
	}
}

// F45: an explicit `concurrency: 3` survives Load unchanged — the loader default only
// fills a nil (omitted) pointer, never overwrites an explicit value.
func TestLoadPreservesExplicitMapConcurrency(t *testing.T) {
	dir := t.TempDir()
	wfPath := writeWorkflow(t, dir, mapWorkflowYAML("root", "concurrency: 3\n      "))

	ld, err := Load(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	m := firstMap(t, ld.Workflow)
	if m.Concurrency == nil || *m.Concurrency != 3 {
		t.Fatalf("Concurrency = %v, want pointer to 3 (unchanged)", m.Concurrency)
	}
}

// F45: the desugar default reaches every IMPORTED module too, not just the root — the
// loader walks ld.Modules (which includes the root under id ""), so a map inside an
// imported workflow with an omitted concurrency: is defaulted exactly like a root map.
func TestLoadDefaultsOmittedMapConcurrencyInImportedModule(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "modules"))
	writeFile(t, filepath.Join(dir, "modules", "child.awf.yaml"), mapWorkflowYAML("child", ""))
	rootPath := writeWorkflow(t, dir, "workflow: root\nversion: 1\nimports:\n  child: modules/child.awf.yaml\ncontainers: {}\ngraph: []\n")

	ld, err := Load(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	child, ok := ld.Module("child")
	if !ok {
		t.Fatal("missing child module")
	}
	m := firstMap(t, child.Workflow)
	if m.Concurrency == nil || *m.Concurrency != 1 {
		t.Fatalf("imported module's map Concurrency = %v, want pointer to 1 (default)", m.Concurrency)
	}
}

// F45: since the loader default runs BEFORE any digest computation, an omitted
// `concurrency:` and an explicit `concurrency: 1` must normalize to byte-identical IR —
// same content digest. This is the load-bearing invariant: "default an OMITTED field"
// only holds if it's actually invisible downstream.
func TestLoadDigestOmittedMapConcurrencyEqualsExplicitOne(t *testing.T) {
	dirOmitted := t.TempDir()
	omittedPath := writeWorkflow(t, dirOmitted, mapWorkflowYAML("same", ""))
	ldOmitted, err := Load(omittedPath)
	if err != nil {
		t.Fatal(err)
	}
	digestOmitted, err := ldOmitted.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}

	dirExplicit := t.TempDir()
	explicitPath := writeWorkflow(t, dirExplicit, mapWorkflowYAML("same", "concurrency: 1\n      "))
	ldExplicit, err := Load(explicitPath)
	if err != nil {
		t.Fatal(err)
	}
	digestExplicit, err := ldExplicit.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}

	if digestOmitted != digestExplicit {
		t.Fatalf("digest(omitted concurrency) = %s, digest(concurrency: 1) = %s, want equal", digestOmitted, digestExplicit)
	}
}

// loadImportedMapDigest writes a root workflow that imports one child module holding a
// single map, and returns the whole definition's content digest. The two ends differ ONLY
// in the CHILD module's concurrency line — the root is byte-identical — so the returned
// digests isolate whether an imported module's omitted-vs-explicit concurrency: normalizes
// identically (the desugar runs per-module, not just on the root).
func loadImportedMapDigest(t *testing.T, childConcurrencyLine string) string {
	t.Helper()
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "modules"))
	writeFile(t, filepath.Join(dir, "modules", "child.awf.yaml"), mapWorkflowYAML("child", childConcurrencyLine))
	rootPath := writeWorkflow(t, dir, "workflow: root\nversion: 1\nimports:\n  child: modules/child.awf.yaml\ncontainers: {}\ngraph: []\n")

	ld, err := Load(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// F45: the digest-equality invariant must hold for an IMPORTED module's map, not only a
// root-level one. Two definitions identical except the child module's concurrency (omitted
// vs explicit `concurrency: 1`) must produce the same whole-definition digest — pinning that
// the per-module desugar (loader.Load iterating every module) can't silently diverge an
// imported module's digest. Mirrors TestLoadDigestOmittedMapConcurrencyEqualsExplicitOne but
// with the map one import level down.
func TestLoadDigestOmittedMapConcurrencyInImportedModuleEqualsExplicitOne(t *testing.T) {
	digestOmitted := loadImportedMapDigest(t, "")
	digestExplicit := loadImportedMapDigest(t, "concurrency: 1\n      ")

	if digestOmitted != digestExplicit {
		t.Fatalf("digest(imported map, omitted concurrency) = %s, digest(imported map, concurrency: 1) = %s, want equal", digestOmitted, digestExplicit)
	}
}
