package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/clock"
)

// writeValidWorkflow writes a minimal valid workflow and returns its path.
func writeValidWorkflow(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wf.yaml")
	body := `id: demo
version: 1
graph:
  - id: hello
    run: "echo hi"
    container: main
containers:
  main:
    image: "alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000"
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runArgs(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	r := &Runner{IDGen: clock.CryptoIDGen{}}
	rc := r.Run(args, &out, &errb)
	return rc, out.String(), errb.String()
}

// TestArgOrderingFlagsEitherSide asserts every command accepts flags on the
// side of its positionals that the pre-pflag parser rejected. After the pflag
// migration, parsing is GNU-interspersed: a flag is accepted before AND after
// positionals. We probe by asserting the usage banner is NOT printed (parse
// succeeded); the command then fails downstream (not-found / unprogrammed
// backend / load error), which is fine — we only assert the flag was accepted.
func TestArgOrderingFlagsEitherSide(t *testing.T) {
	wf := writeValidWorkflow(t)
	rid := "nope-no-such-run"

	cases := []struct {
		name string
		args []string
	}{
		// Commands that REQUIRED flags-before-positional (stdlib flag): test flags AFTER.
		{"run flags-after", []string{"run", wf, "--backend", "fake", "--run-id", "t1", "--state-dir", t.TempDir()}},
		{"validate flags-after", []string{"validate", wf, "--format", "json"}},
		{"resume flags-after", []string{"resume", rid, wf, "--state-dir", t.TempDir()}},
		{"signal flags-after", []string{"signal", rid, "sig", "--payload", "{}", "--state-dir", t.TempDir()}},
		{"pause flags-after", []string{"pause", rid, "--reason", "x", "--state-dir", t.TempDir()}},
		{"cancel flags-after", []string{"cancel", rid, "--reason", "x", "--state-dir", t.TempDir()}},
		// Commands that REQUIRED positional-first (parseSinglePositional): test flags BEFORE.
		{"inspect flags-before", []string{"inspect", "--tokens", rid, "--state-dir", t.TempDir()}},
		{"trace flags-before", []string{"trace", "--output", "json", rid, "--state-dir", t.TempDir()}},
		{"outputs flags-before", []string{"outputs", "--workflow", wf, rid, "--state-dir", t.TempDir()}},
		{"graph flags-before", []string{"graph", "--output", "json", wf}},
		{"ui flags-before", []string{"ui", "--state-dir", t.TempDir(), filepath.Join(t.TempDir(), "nope.yaml")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, errs := runArgs(tc.args...)
			if strings.Contains(errs, "usage:") {
				t.Fatalf("%s: parse rejected (usage banner printed); flag not accepted on this side.\nstderr:\n%s", tc.name, errs)
			}
		})
	}
}

// TestArgOrderingDashTerminatorAndArity covers the `--` end-of-flags terminator
// and wrong-arity rejection from the acceptance matrix (cases d and e).
func TestArgOrderingDashTerminatorAndArity(t *testing.T) {
	// `--` ends flag parsing: a positional that begins with `-` is taken literally.
	_, _, errs := runArgs("inspect", "--state-dir", t.TempDir(), "--", "-weirdrun")
	if strings.Contains(errs, "usage:") {
		t.Fatalf("inspect `--` terminator rejected:\n%s", errs)
	}
	if !strings.Contains(errs, "-weirdrun") {
		t.Fatalf("inspect did not treat `-weirdrun` as a positional after `--`:\n%s", errs)
	}

	// Wrong arity still errors with a usage banner (ls takes zero positionals).
	rc, _, errs2 := runArgs("ls", "extra", "--state-dir", t.TempDir())
	if rc != ExitUsage || !strings.Contains(errs2, "usage:") {
		t.Fatalf("ls with a stray positional should be a usage error; rc=%d stderr=%q", rc, errs2)
	}
}
