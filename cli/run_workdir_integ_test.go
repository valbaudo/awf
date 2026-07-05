//go:build integ

package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
)

// This test MUST stay integ-tagged. It drives a REAL `awf run --backend native`
// (no fake backend injected), so on landlock-capable, bwrap-less hosts (e.g.
// GitHub ubuntu-latest CI) the native backend confines each step by re-execing
// the test binary as `<self> __sandbox <policy> -- sh -c <run>`. That re-exec
// only lands on `sh -c` when the cli test binary intercepts "__sandbox" via
// native.MaybeRunSandboxTrampoline — which cli/trampoline_main_linux_test.go
// wires up in TestMain, and which is itself `//go:build linux && integ`. Under
// a plain `make test` (no integ tag) that TestMain is absent, the re-exec'd
// binary re-runs the suite instead of the step, the step never commits, and the
// run retries to exhaustion. Non-integ cli tests must inject a fake backend and
// never reach the sandbox path (see the trampoline TestMain's doc). This is why
// the F26/U3 regression lives here, alongside run_backend_integ_test.go.

// TestRun_WorkdirIsPerRun is the F26/U3 regression: two real `awf run`
// invocations against the same --state-dir must get disjoint native workdir
// roots (work/<run-id>/...), not a single shared work/ subtree. This is only
// observable on the PRODUCTION newBackend construction path — r.Backend
// (test-injected fake) ignores workdirRoot entirely, and resolveBackend
// returns an injected Backend as-is without ever consulting workdirRoot. So
// this test deliberately does NOT use newTestRunner: it leaves Runner.Backend
// nil and drives --backend native so cli/run.go actually constructs a native
// container.Backend rooted at the workdirRoot this test is asserting on.
//
// The workflow's single step ("true") does no filesystem work of its own;
// the assertion is purely on the on-disk workdir layout newBackend/native.Create
// leave behind. Native's Destroy (deferred teardown) os.RemoveAlls only the
// per-CONTAINER dir (workdirRoot/<container>), not the per-RUN workdirRoot
// itself, so work/<run-id>/ survives a normal run's cleanup as an (empty)
// directory — see container/native/backend.go Create/Destroy.
func TestRun_WorkdirIsPerRun(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	wfPath := writeMinimalWorkflow(t, t.TempDir())
	runner := &cli.Runner{IDGen: &clock.Fake{IDs: []string{"test-run-1", "test-run-2"}}}

	for _, runID := range []string{"test-run-1", "test-run-2"} {
		var stdout, stderr bytes.Buffer
		rc := runner.Run(
			[]string{"run", "--state-dir", stateDir, "--backend", "native", wfPath},
			&stdout, &stderr,
		)
		if rc != cli.ExitOK {
			t.Fatalf("run (want id %s): rc = %d, want ExitOK; stderr: %s", runID, rc, stderr.String())
		}
	}

	entries, err := os.ReadDir(filepath.Join(stateDir, "work"))
	if err != nil {
		t.Fatalf("ReadDir(work): %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected per-run workdirs under work/, got %d entries: %v", len(entries), entries)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("work/%s is not a per-run dir", e.Name())
		}
		seen[e.Name()] = true
	}
	for _, want := range []string{"test-run-1", "test-run-2"} {
		if !seen[want] {
			t.Errorf("work/%s missing; got entries %v (per-run-id roots not disjoint)", want, entries)
		}
	}
}
