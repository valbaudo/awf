package loader

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadError_DetailKeepsWrappedErr(t *testing.T) {
	e := &LoadError{Code: "AWF_IMPORT_DECODE", Message: "workflow failed to decode",
		Err: errors.New("[9:10] mapping value is not allowed in this context")}
	got := e.Error()
	if !strings.Contains(got, "mapping value is not allowed") {
		t.Fatalf("wrapped goccy detail dropped: %q", got)
	}
	if !strings.Contains(got, "workflow failed to decode") {
		t.Fatalf("message dropped: %q", got)
	}
}
