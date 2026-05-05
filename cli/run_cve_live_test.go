//go:build integ && live

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	dockerclient "github.com/docker/docker/client"

	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// This file holds the only cli/ integ test that spends Anthropic API
// money — it runs a real `claude` agent step. It carries the extra
// `live` build constraint so the CI `make integ` target (built with
// -tags=integ only) never compiles it. `make integ-live` builds with
// -tags='integ live' to run it locally, by hand, when touching the
// claude adapter or bumping its pinned version. The other tests in
// run_backend_integ_test.go are docker/native-only (no API cost) and
// stay on the plain `integ` tag so CI keeps exercising real Backend.Exec
// plumbing for free. The shared helpers pullImage /
// registerComposeProjectCleanup live in run_backend_integ_test.go
// (same package, also compiled under -tags='integ live').

// TestCLIRunCVEPipelineRealDockerThroughFirstAgentStep (slice 5.4
// rename of Phase 4.5's TestCLIRunCVEPipelineRealDockerToFirstAgentStep).
//
// Phase 4.5 asserted the run errored at the first agent step because
// the dispatcher rejected AgentStep with ErrNodeNotImplemented. Slice 5.2
// closed that arm; slice 5.3 shipped the Claude adapter. This slice
// flips the assertion: the first agent step (triage) now COMPLETES
// under real claude with typed Output the downstream `if` guard reads.
//
// Skips when claude not on PATH, no auth env, OR docker unreachable.
func TestCLIRunCVEPipelineRealDockerThroughFirstAgentStep(t *testing.T) {
	dockerCli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}
	defer func() { _ = dockerCli.Close() }()
	if _, perr := dockerCli.Ping(context.Background()); perr != nil {
		t.Skipf("docker ping: %v", perr)
	}

	authEnv := map[string]string{}
	for _, name := range claude.DefaultEnvAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			authEnv[name] = v
		}
	}
	if len(authEnv) == 0 {
		t.Skipf("no Claude auth env var present (set one of %v)", claude.DefaultEnvAllowlist)
	}

	pullImage(t, dockerCli, alpineDigest)

	stateDir := t.TempDir()
	runID := fmt.Sprintf("cve-real-docker-%d", time.Now().UnixNano())
	registerComposeProjectCleanup(t, dockerCli, runID)

	// Production code path: AgentEnv populated; no test-injected
	// Resolver. cli/run.go builds the claude registry from --agent-env
	// at startup (slice 5.3).
	runner := &cli.Runner{
		IDGen:    &clock.Fake{IDs: []string{runID}},
		AgentEnv: claude.DefaultEnvAllowlist,
	}

	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{"run", "--state-dir", stateDir, "--backend", "docker",
			"--input", `{"cve_id":"CVE-9999-0001"}`,
			"testdata/phase3/cve-pipeline.yaml"},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d; want %d (triage should complete; downstream skip is OK)\nstdout:\n%s\nstderr:\n%s",
			rc, cli.ExitOK, stdout.String(), stderr.String())
	}

	// Read the log + verify step.triage completed with typed outputs.
	logPath := filepath.Join(stateDir, "runs", runID, "log")
	events, ferr := state.FoldFile(logPath)
	if ferr != nil {
		t.Fatalf("state.FoldFile(%s): %v", logPath, ferr)
	}
	var triageCompleted *state.Event
	for i, ev := range events {
		// state.Event field is Path (not NodePath) — verified state/event.go:23-32.
		if ev.Type == "node.completed" && ev.Path == "triage" {
			triageCompleted = &events[i]
			break
		}
	}
	if triageCompleted == nil {
		t.Fatalf("node.completed for 'triage' not found; events: %v", eventPaths(events))
	}
	outputs := readNodeCompletedOutputs(t, stateDir, triageCompleted)
	if _, ok := outputs["web_exploitable"].(bool); !ok {
		t.Errorf("triage.web_exploitable not bool; got %T (%v)", outputs["web_exploitable"], outputs["web_exploitable"])
	}
	if _, ok := outputs["has_source"].(bool); !ok {
		t.Errorf("triage.has_source not bool; got %T (%v)", outputs["has_source"], outputs["has_source"])
	}
}

// readNodeCompletedOutputs decodes the node.completed event's
// OutputsRef and reads the resulting blob from <stateDir>/blobs.
//
// state.Event.Data is the inline json.RawMessage payload — verified
// state/event.go:23-32. state.OpenBlobs returns *FSBlobs which has no
// Close method (verified state/blobs.go) — the FS-backed store has no
// long-lived handles to release, so there's nothing to defer.
func readNodeCompletedOutputs(t *testing.T, stateDir string, ev *state.Event) map[string]any {
	t.Helper()
	var data engine.NodeCompletedData
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("unmarshal NodeCompletedData: %v", err)
	}
	blobs, err := state.OpenBlobs(filepath.Join(stateDir, "blobs"))
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	raw, err := blobs.Get(data.OutputsRef)
	if err != nil {
		t.Fatalf("blobs.Get(%s): %v", data.OutputsRef, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal outputs: %v", err)
	}
	return out
}

func eventPaths(events []state.Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Type+"@"+ev.Path)
	}
	return out
}
