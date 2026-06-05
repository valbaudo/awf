package engine

import (
	"encoding/json"
	"testing"

	"github.com/valbaudo/awf/state"
)

func TestFoldMapItemCarriesImageDigestAndReason(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	started, _ := json.Marshal(RunStartedData{RunID: "r", WorkflowDigest: "d"})
	passed, _ := json.Marshal(MapItemData{N: 0, Status: ItemPassed, ImageDigest: "registry.example.com/app@sha256:aaa"})
	failed, _ := json.Marshal(MapItemData{N: 1, Status: ItemFailed, Reason: "image_unavailable"})
	events := []state.Event{
		{Type: EventRunStarted, Data: started},
		{Type: EventMapItem, Path: "map[0]", Data: passed},
		{Type: EventMapItem, Path: "map[0]", Data: failed},
	}
	rs, err := Fold(events, blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	got := rs.LookupMapItems("map[0]")
	if len(got) != 2 {
		t.Fatalf("MapItems len = %d, want 2", len(got))
	}
	byN := map[int]MapItemRecord{}
	for _, mr := range got {
		byN[mr.N] = mr
	}
	if byN[0].ImageDigest != "registry.example.com/app@sha256:aaa" {
		t.Errorf("N=0 ImageDigest = %q, want the captured digest", byN[0].ImageDigest)
	}
	if byN[1].Reason != "image_unavailable" {
		t.Errorf("N=1 Reason = %q, want image_unavailable", byN[1].Reason)
	}
}
