package codex_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent/codex"
)

func TestErrUnexpectedExit_Message(t *testing.T) {
	e := &codex.ErrUnexpectedExit{ExitCode: 1, Output: "codex produced no agent message"}
	if !strings.Contains(e.Error(), "no usable result") {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestErrStreamParse_Unwrap(t *testing.T) {
	cause := errors.New("boom")
	e := &codex.ErrStreamParse{Line: []byte("x"), Cause: cause}
	if !errors.Is(e, cause) {
		t.Errorf("errors.Is(ErrStreamParse, cause) = false")
	}
}

func TestErrSessionReuseAttempted_Message(t *testing.T) {
	e := &codex.ErrSessionReuseAttempted{Key: "resume"}
	if !strings.Contains(e.Error(), "resume") {
		t.Errorf("Error() = %q", e.Error())
	}
}
