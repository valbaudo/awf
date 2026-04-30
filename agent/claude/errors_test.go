package claude

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrBareRequiresAPIKey_FormatsAvailable(t *testing.T) {
	e := &ErrBareRequiresAPIKey{AvailableKeys: []string{"OTHER", "ANOTHER"}}
	msg := e.Error()
	if !strings.Contains(msg, "OTHER") || !strings.Contains(msg, "ANOTHER") {
		t.Errorf("Error() = %q; want list of available keys", msg)
	}
	if !strings.Contains(msg, "ANTHROPIC_API_KEY") {
		t.Errorf("Error() = %q; want mention of required env-var name", msg)
	}
}

func TestErrBareRequiresAPIKey_FormatsEmpty(t *testing.T) {
	e := &ErrBareRequiresAPIKey{}
	if !strings.Contains(e.Error(), "empty") {
		t.Errorf("Error() = %q; want mention of empty allowlist", e.Error())
	}
}

func TestErrSessionReuseAttempted_NamesKey(t *testing.T) {
	e := &ErrSessionReuseAttempted{Key: "session_id"}
	if !strings.Contains(e.Error(), `"session_id"`) {
		t.Errorf("Error() = %q; want offending key quoted", e.Error())
	}
}

func TestErrStreamParse_TruncatesLongLine(t *testing.T) {
	long := strings.Repeat("a", 500)
	e := &ErrStreamParse{Line: []byte(long), Cause: fmt.Errorf("bad json")}
	msg := e.Error()
	if strings.Count(msg, "a") > 250 {
		t.Errorf("Error() did not truncate long line; len=%d", len(msg))
	}
}

func TestErrStreamParse_UnwrapsCause(t *testing.T) {
	cause := errors.New("bad json")
	e := &ErrStreamParse{Line: []byte("x"), Cause: cause}
	if !errors.Is(e, cause) {
		t.Errorf("errors.Is failed for cause %v", cause)
	}
}

func TestErrUnexpectedExit_FormatsExitCode(t *testing.T) {
	e := &ErrUnexpectedExit{ExitCode: 137, Stderr: "killed"}
	msg := e.Error()
	if !strings.Contains(msg, "137") {
		t.Errorf("Error() = %q; want exit code in message", msg)
	}
	if !strings.Contains(msg, "killed") {
		t.Errorf("Error() = %q; want stderr preview", msg)
	}
}

func TestErrAgentRuntimeNotFound_NamesContainer(t *testing.T) {
	cause := errors.New("exit code 127")
	e := &ErrAgentRuntimeNotFound{Ref: AdapterRef, Container: "lab", Cause: cause}
	msg := e.Error()
	if !strings.Contains(msg, "lab") {
		t.Errorf("Error() = %q; want container name", msg)
	}
	if !errors.Is(e, cause) {
		t.Errorf("Unwrap failed")
	}
}

func TestAdapterRef_Constant(t *testing.T) {
	if AdapterRef != "anthropic/claude-code" {
		t.Errorf("AdapterRef = %q; want %q (matches Phase 5 design + Appendix A CVE-pipeline literal)", AdapterRef, "anthropic/claude-code")
	}
}
