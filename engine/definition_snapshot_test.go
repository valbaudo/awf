package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

// The definition snapshot is the run's own copy of its canonical definition, stored once at run
// start so a viewer (e.g. `awf ui`) can render a past run faithfully after the on-disk file
// changes. These tests pin the round-trip (store → load reconstructs byte-identically), CAS
// dedup, and the nil guard.

func TestStoreAndLoadRunStartedDefinitionSnapshotRoundTrip(t *testing.T) {
	ld, err := loader.Load("../loader/testdata/valid/cve-pipeline.yaml")
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}
	blobs := state.NewInMemoryBlobs()

	ref, err := StoreRunStartedDefinitionSnapshot(blobs, ld)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if ref == "" {
		t.Fatal("store returned empty ref")
	}

	got, err := LoadRunStartedDefinitionSnapshot(blobs, ref)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want, err := json.Marshal(ld)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	gotBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal reconstructed: %v", err)
	}
	if string(want) != string(gotBytes) {
		t.Fatalf("round-trip mismatch:\nwant %s\ngot  %s", want, gotBytes)
	}
}

func TestStoreRunStartedDefinitionSnapshotDedup(t *testing.T) {
	ld, err := loader.Load("../loader/testdata/valid/cve-pipeline.yaml")
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}
	blobs := state.NewInMemoryBlobs()
	r1, err := StoreRunStartedDefinitionSnapshot(blobs, ld)
	if err != nil {
		t.Fatalf("store 1: %v", err)
	}
	r2, err := StoreRunStartedDefinitionSnapshot(blobs, ld)
	if err != nil {
		t.Fatalf("store 2: %v", err)
	}
	if r1 != r2 {
		t.Fatalf("identical definition must content-address to one blob: %q != %q", r1, r2)
	}
}

func TestStoreRunStartedDefinitionSnapshotNil(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	ref, err := StoreRunStartedDefinitionSnapshot(blobs, nil)
	if err != nil {
		t.Fatalf("nil definition: unexpected error %v", err)
	}
	if ref != "" {
		t.Fatalf("nil definition: want empty ref, got %q", ref)
	}
}

func TestRunStartedDataDefinitionRefJSONOmitempty(t *testing.T) {
	// Present when set; absent (omitempty) when empty, so pre-snapshot golden logs are unchanged.
	withRef, err := json.Marshal(RunStartedData{DefinitionRef: "awf-d1:sha256:abc"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(withRef), `"definition_ref":"awf-d1:sha256:abc"`) {
		t.Fatalf("definition_ref not emitted: %s", withRef)
	}
	without, err := json.Marshal(RunStartedData{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(without), "definition_ref") {
		t.Fatalf("empty definition_ref must be omitted: %s", without)
	}
}
