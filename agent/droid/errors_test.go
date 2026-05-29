package droid

import (
	"errors"
	"strings"
	"testing"
)

func TestErrSessionReuseAttempted_Message(t *testing.T) {
	e := &ErrSessionReuseAttempted{Key: "resume"}
	if !strings.Contains(e.Error(), "resume") {
		t.Errorf("Error() = %q, want it to mention the key", e.Error())
	}
}

func TestErrRuntimeNotFound_Unwrap(t *testing.T) {
	cause := errors.New("boom")
	e := &ErrRuntimeNotFound{Ref: AdapterRef, Container: "lab", Cause: cause}
	if !errors.Is(e, cause) {
		t.Error("errors.Is(e, cause) = false, want true (Unwrap)")
	}
}
