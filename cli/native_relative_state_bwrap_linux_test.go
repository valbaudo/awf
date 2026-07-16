//go:build linux

package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
)

// TestCLIRunNativeRelativeDefaultStateUsesAbsoluteBwrapPaths is the regression
// for Linux bwrap rejecting a relative bind source. It exercises production
// sandbox detection with a functional recording bwrap, then inspects the real
// launch argv; Landlock or the warned no-op fallback cannot make this pass.
func TestCLIRunNativeRelativeDefaultStateUsesAbsoluteBwrapPaths(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve temporary cwd: %v", err)
	}
	recordPath := filepath.Join(root, "bwrap.argv")
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bwrapPath := filepath.Join(binDir, "bwrap")
	const recordingBwrap = `#!/bin/sh
{
  printf '%s\n' BEGIN
  printf '%s\n' "$@"
  printf '%s\n' END
} >> "$AWF_BWRAP_RECORD"
chdir=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --chdir)
      chdir=$2
      shift 2
      ;;
    --)
      shift
      if [ -n "$chdir" ]; then
        cd "$chdir" || exit 97
      fi
      exec "$@"
      ;;
    *)
      shift
      ;;
  esac
done
exit 98
`
	if err := os.WriteFile(bwrapPath, []byte(recordingBwrap), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AWF_BWRAP_RECORD", recordPath)
	t.Setenv("AWF_STATE_DIR", "")

	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldCWD); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	})

	wfPath := filepath.Join(root, "relative-state.yaml")
	if err := os.WriteFile(wfPath, []byte(`workflow: relative-native
version: 1
containers: {}
graph:
  - id: scratch
    run: "printf native-ok > scratch.txt"
    output_files: [scratch.txt]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := &cli.Runner{IDGen: &clock.Fake{IDs: []string{"relative-native-run"}}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--backend", "native", wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stdout=%q stderr=%q", rc, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "native sandbox: bwrap") {
		t.Fatalf("test did not execute the bwrap path; stderr=%q", stderr.String())
	}
	if strings.Contains(stderr.String(), "invalid source path") {
		t.Fatalf("relative bind source reached bwrap: %q", stderr.String())
	}

	recorded, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read bwrap argv: %v", err)
	}
	invocations := strings.Split(strings.TrimSpace(string(recorded)), "END\nBEGIN\n")
	if len(invocations) < 2 {
		t.Fatalf("bwrap invocations = %d, want probe plus dispatch; record=%q", len(invocations), recorded)
	}
	dispatch := strings.TrimPrefix(invocations[len(invocations)-1], "BEGIN\n")
	lines := strings.Split(strings.TrimSuffix(dispatch, "\nEND"), "\n")
	var bindSource, bindDest, chdir string
	for i := 0; i < len(lines); i++ {
		switch lines[i] {
		case "--bind":
			if i+2 < len(lines) {
				bindSource, bindDest = lines[i+1], lines[i+2]
			}
		case "--chdir":
			if i+1 < len(lines) {
				chdir = lines[i+1]
			}
		}
	}
	for name, got := range map[string]string{"bind source": bindSource, "bind destination": bindDest, "chdir": chdir} {
		if got == "" || !filepath.IsAbs(got) {
			t.Errorf("%s = %q, want canonical absolute path; dispatch argv=%q", name, got, dispatch)
		}
	}
	// Destroy removes the per-container workdir before Runner.Run returns, so
	// derive it from the already-canonical cwd rather than resolving it after
	// teardown.
	wantWorkdir := filepath.Join(canonicalRoot, ".awf", "work", "relative-native-run", "__awf_host_workspace")
	if bindSource != wantWorkdir || bindDest != wantWorkdir || chdir != wantWorkdir {
		t.Fatalf("bwrap workdir paths = bind %q -> %q, chdir %q; want %q", bindSource, bindDest, chdir, wantWorkdir)
	}
	if !strings.Contains(stdout.String(), "run relative-native-run: ok") {
		t.Fatalf("successful scratch write not reported: stdout=%q", stdout.String())
	}
}
