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

// The preview must show the TAIL of codex's output: fatal errors come last —
// codex prints benign startup warnings first ("could not create PATH aliases",
// "stale arg0 temp dirs"), and a head-first preview let the warning consume the
// whole budget, hiding the real 401 (prestige run 91db8db6, 2026-08-16).
func TestErrUnexpectedExit_PreviewShowsTail(t *testing.T) {
	head := strings.Repeat("W", 300) // a wall of warning noise
	fatal := "turn.failed: 401 Unauthorized"
	e := &codex.ErrUnexpectedExit{ExitCode: 1, Output: head + fatal}
	msg := e.Error()
	if !strings.Contains(msg, fatal) {
		t.Errorf("preview must contain the fatal tail, got: %q", msg)
	}
	if strings.Contains(msg, strings.Repeat("W", 300)) {
		t.Errorf("preview must not be head-capped at the warning wall, got: %q", msg)
	}
}
