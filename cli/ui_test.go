package cli

import (
	"bytes"
	"testing"
)

func TestUINoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := cliUI(nil, &out, &errb); rc != ExitUsage {
		t.Fatalf("ui no-args rc = %d, want ExitUsage", rc)
	}
}

func TestUIBadPath(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := cliUI([]string{"testdata/does-not-exist.yaml"}, &out, &errb); rc != ExitUsage {
		t.Fatalf("ui bad-path rc = %d, want ExitUsage", rc)
	}
}
