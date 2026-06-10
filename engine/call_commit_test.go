package engine

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/state"
)

func TestCommitCallProductRejectsMissingExportedFileRef(t *testing.T) {
	log := state.NewInMemoryLog(clock.System{})
	blobs := state.NewInMemoryBlobs()

	_, err := commitCallProduct(log, blobs, "scan", WorkflowExportResult{
		Outputs: map[string]any{"summary": "ok"},
		Files:   map[string]string{"report": "awf-d1:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	if err == nil {
		t.Fatal("commitCallProduct succeeded, want missing exported file ref error")
	}
	events, ferr := log.Fold()
	if ferr != nil {
		t.Fatalf("log.Fold: %v", ferr)
	}
	for _, ev := range events {
		if ev.Type == EventNodeCompleted {
			t.Fatalf("unexpected node.completed after missing exported file ref: %+v", ev)
		}
	}
}

func TestCommitCallProductStoresOutputsAndAppendsNodeCompleted(t *testing.T) {
	log := state.NewInMemoryLog(clock.System{})
	blobs := state.NewInMemoryBlobs()
	fileRef, err := blobs.Put([]byte("report"))
	if err != nil {
		t.Fatalf("Blobs.Put file: %v", err)
	}

	nr, err := commitCallProduct(log, blobs, "scan", WorkflowExportResult{
		Outputs: map[string]any{"summary": "ok"},
		Files:   map[string]string{"report": fileRef},
	})
	if err != nil {
		t.Fatalf("commitCallProduct: %v", err)
	}
	if nr.Outcome != OutcomeOK || nr.OutputsRef == "" || nr.Files["report"] != fileRef {
		t.Fatalf("NodeResult = %+v, want ok with outputs ref and exported file", nr)
	}
	raw, err := blobs.Get(nr.OutputsRef)
	if err != nil {
		t.Fatalf("Blobs.Get outputs: %v", err)
	}
	var outputs map[string]any
	if err := json.Unmarshal(raw, &outputs); err != nil {
		t.Fatalf("unmarshal outputs blob: %v", err)
	}
	if outputs["summary"] != "ok" {
		t.Fatalf("outputs blob summary = %v, want ok", outputs["summary"])
	}

	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	if len(events) != 1 || events[0].Type != EventNodeCompleted {
		t.Fatalf("events = %+v, want one node.completed", events)
	}
}

func TestNodeCompletedAppendAllowlist(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	var offenders []string
	for _, filename := range files {
		base := filepath.Base(filename)
		if strings.HasSuffix(base, "_test.go") || base == "commit.go" {
			continue
		}
		file, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile %s: %v", filename, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isStateEventComposite(lit) || !setsNodeCompletedType(lit) {
				return true
			}
			pos := fset.Position(lit.Pos())
			offenders = append(offenders, pos.String())
			return true
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("node.completed state.Event construction outside commit.go: %v", offenders)
	}
}

func isStateEventComposite(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Event" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "state"
}

func setsNodeCompletedType(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Type" {
			continue
		}
		value, ok := kv.Value.(*ast.Ident)
		return ok && value.Name == "EventNodeCompleted"
	}
	return false
}
