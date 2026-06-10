package engine

import (
	"errors"
	"testing"
)

func TestCheckRuntimesDriftVersionChangedIsTyped(t *testing.T) {
	recorded := []ResolvedRuntime{{Ref: "x", Version: "v1", Container: "lab"}}
	current := []ResolvedRuntime{{Ref: "x", Version: "v2", Container: "lab"}}

	err := CheckRuntimesDrift(recorded, current)
	var drift *ErrRuntimeDrift
	if !errors.As(err, &drift) {
		t.Fatalf("err = %v, want *ErrRuntimeDrift", err)
	}
	if drift.Ref != "x" || drift.Container != "lab" || drift.Recorded != "v1" || drift.Current != "v2" {
		t.Fatalf("drift = %+v, want x/lab v1->v2", drift)
	}
}
