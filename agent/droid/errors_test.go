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

func TestErrMissingAPIKey_Message(t *testing.T) {
	if got := (&ErrMissingAPIKey{}).Error(); !strings.Contains(got, "empty") {
		t.Errorf("empty-allowlist Error() = %q, want it to mention the allowlist is empty", got)
	}
	got := (&ErrMissingAPIKey{AvailableKeys: []string{"ANTHROPIC_API_KEY"}}).Error()
	if !strings.Contains(got, "FACTORY_API_KEY") || !strings.Contains(got, "ANTHROPIC_API_KEY") {
		t.Errorf("populated Error() = %q, want it to name the required + available keys", got)
	}
}

func TestErrUnexpectedExit_Message(t *testing.T) {
	got := (&ErrUnexpectedExit{ExitCode: 137, Stderr: "killed"}).Error()
	if !strings.Contains(got, "137") || !strings.Contains(got, "killed") {
		t.Errorf("Error() = %q, want it to contain the exit code and stderr", got)
	}
}

func TestErrStreamParse_TruncatesLongLine(t *testing.T) {
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	got := (&ErrStreamParse{Line: long, Cause: errors.New("boom")}).Error()
	if len(got) > 400 { // 200-byte preview cap + a short fixed prefix/suffix
		t.Errorf("Error() length = %d, want the line truncated to the preview cap", len(got))
	}
}
