package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/valbaudo/awf/clock"
)

// TestValidateFormatAliasMatchesOutput asserts the deprecated --format alias
// produces byte-identical stdout to the canonical --output, that --output keeps
// stderr clean (so `| jq` works), and that --format emits a deprecation notice
// to stderr ONLY (not stdout).
func TestValidateFormatAliasMatchesOutput(t *testing.T) {
	wf := writeValidWorkflow(t)

	var o1, e1, o2, e2 bytes.Buffer
	rc1 := (&Runner{IDGen: clock.CryptoIDGen{}}).Run([]string{"validate", "--output", "json", wf}, &o1, &e1)
	rc2 := (&Runner{IDGen: clock.CryptoIDGen{}}).Run([]string{"validate", "--format", "json", wf}, &o2, &e2)

	if rc1 != rc2 {
		t.Fatalf("rc differ: --output=%d --format=%d", rc1, rc2)
	}
	if o1.String() != o2.String() {
		t.Fatalf("stdout differs between --output and --format alias:\n--output:\n%s\n--format:\n%s", o1.String(), o2.String())
	}
	if e1.Len() != 0 {
		t.Errorf("--output left stderr non-empty (breaks `| jq`): %q", e1.String())
	}
	if !strings.Contains(e2.String(), "deprecated") {
		t.Errorf("--format did not emit a deprecation notice on stderr: %q", e2.String())
	}
	if strings.Contains(o2.String(), "deprecated") {
		t.Errorf("deprecation notice leaked onto stdout: %q", o2.String())
	}
}

// TestValidateFormatHiddenFromHelp asserts the deprecated alias is not advertised
// in the help/usage output, while --output is.
func TestValidateFormatHiddenFromHelp(t *testing.T) {
	var out, errb bytes.Buffer
	rc := (&Runner{IDGen: clock.CryptoIDGen{}}).Run([]string{"validate", "--help"}, &out, &errb)
	if rc != ExitOK {
		t.Fatalf("validate --help rc=%d, stderr=%q", rc, errb.String())
	}
	if strings.Contains(out.String(), "--format") {
		t.Errorf("help advertises the deprecated --format alias: %q", out.String())
	}
	if !strings.Contains(out.String(), "--output") {
		t.Errorf("help does not mention canonical --output: %q", out.String())
	}
}

// TestOutputShorthand asserts the -o shorthand is accepted on every format-bearing
// command. For commands that fail downstream (no such run), we assert the parser
// accepted -o: no usage banner and no "unknown shorthand" error.
func TestOutputShorthand(t *testing.T) {
	wf := writeValidWorkflow(t)
	rid := "nope-no-such-run"

	cases := []struct {
		name string
		args []string
	}{
		{"validate", []string{"validate", "-o", "json", wf}},
		{"ls", []string{"ls", "-o", "json", "--state-dir", t.TempDir()}},
		{"inspect", []string{"inspect", "-o", "json", rid, "--state-dir", t.TempDir()}},
		{"trace", []string{"trace", "-o", "json", rid, "--state-dir", t.TempDir()}},
		{"graph", []string{"graph", "-o", "json", wf}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			(&Runner{IDGen: clock.CryptoIDGen{}}).Run(tc.args, &out, &errb)
			if strings.Contains(errb.String(), "usage:") || strings.Contains(errb.String(), "unknown shorthand") {
				t.Fatalf("%s: -o shorthand not accepted:\n%s", tc.name, errb.String())
			}
		})
	}
}
