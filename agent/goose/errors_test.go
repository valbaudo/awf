package goose_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent/goose"
)

func TestErrUnexpectedExit_Message(t *testing.T) {
	e := &goose.ErrUnexpectedExit{ExitCode: 0, Output: "goose produced no output (possible unknown model)"}
	if !strings.Contains(e.Error(), "no usable result") {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestErrStreamParse_Unwrap(t *testing.T) {
	cause := errors.New("boom")
	e := &goose.ErrStreamParse{Line: []byte("x"), Cause: cause}
	if !errors.Is(e, cause) {
		t.Errorf("errors.Is(ErrStreamParse, cause) = false")
	}
}
