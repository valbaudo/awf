package engine

import (
	"encoding/json"
	"os"
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
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	var offenders []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "commit.go" {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", file, err)
		}
		if strings.Contains(string(body), "state.Event{Type: EventNodeCompleted") ||
			strings.Contains(string(body), "state.Event{\n\t\tType: EventNodeCompleted") ||
			strings.Contains(string(body), "state.Event{\n\tType: EventNodeCompleted") {
			offenders = append(offenders, file)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("node.completed state.Event construction outside commit.go: %v", offenders)
	}
}
