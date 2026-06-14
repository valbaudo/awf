package engine

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

type containerlessFixture struct {
	scope    *Scope
	wf       *ir.Workflow
	moduleID string
	blobs    state.Blobs
	assets   map[string]RunStartedAsset
}

// newContainerlessFixture builds a Scope where input.files.doc resolves to PDF
// bytes committed in an in-memory Blobs, plus a DIRECTORY asset "bundle"
// registered in the run-start asset manifest. Mirrors the Scope/Blobs/asset
// construction the call-step tests use (NewScopeWithInputAndFiles + a
// content-addressed blob ref).
func newContainerlessFixture(t *testing.T) containerlessFixture {
	t.Helper()
	blobs := state.NewInMemoryBlobs()
	pdf := []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< >>\nendobj\n")
	ref, err := blobs.Put(pdf)
	if err != nil {
		t.Fatalf("Put pdf: %v", err)
	}

	rs := &RunState{
		Assets: map[string]RunStartedAsset{
			QualifiedAssetKey(RootModuleID, "bundle"): {
				DeclaredPath: "bundle",
				IsDir:        true,
				Files: []RunStartedAssetFile{
					{Path: "a.txt", Ref: ref, Size: int64(len(pdf))},
				},
			},
		},
	}
	wf := &ir.Workflow{}
	scope := NewScopeWithInputAndFiles(rs, wf, "", nil, map[string]string{"doc": ref})

	return containerlessFixture{
		scope:    scope,
		wf:       wf,
		moduleID: RootModuleID,
		blobs:    blobs,
		assets:   rs.Assets,
	}
}

func TestResolveContainerlessInputFiles_PDF(t *testing.T) {
	h := newContainerlessFixture(t) // helper: scope with input.files.doc -> %PDF bytes in blobs
	got, err := resolveContainerlessInputFiles(
		map[string]string{"doc": "input.files.doc"},
		h.scope, h.wf, h.moduleID, h.blobs, h.assets)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 file, got %d", len(got))
	}
	if got[0].Name != "doc" || got[0].MIME != "application/pdf" {
		t.Fatalf("got %+v", agent.InputFile{Name: got[0].Name, MIME: got[0].MIME})
	}
}

func TestResolveContainerlessInputFiles_RejectsDirAsset(t *testing.T) {
	h := newContainerlessFixture(t) // fixture also registers a DIRECTORY asset "bundle"
	_, err := resolveContainerlessInputFiles(
		map[string]string{"docs": "asset.bundle"},
		h.scope, h.wf, h.moduleID, h.blobs, h.assets)
	if err == nil {
		t.Fatal("expected a directory asset to be rejected as a single containerless input")
	}
}
