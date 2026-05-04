package conformance

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

// testBucket14cGateUnderRealClaude (slice 5.4 Bucket 14c).
//
// End-to-end: runs gate-repair-cve.yaml against real claude via the
// dockerFactory's compose lab. Asserts the engine machinery runs end-
// to-end — outcome OK + at least one gate.attempt recorded on gate[0].
//
// Does NOT assert "attempts >= 2": that depended on Claude being
// insufficiently smart to one-shot the exploit, which is backwards for
// an offensive-security tool (the test breaks as the model improves).
// The repair/feedback wiring is covered by slice 5.2's unit tests
// against the fake adapter.
//
// NOTE (slice 5.4 spec deviation): the spec wording motivates 14c as a
// "benign-payload oracle e2e." With the constant-secret vulnerability
// in this fixture, oracle-side benign-substitution is meaningless. A
// benign-payload e2e remains a follow-up slice with a fool-prone
// vulnerability design.
//
// Skip condition: env.Spec.Compose == nil (nativeFactory has no
// multi-service lab).
//
// Overall test timeout: governed by `go test -timeout` set in the
// Makefile's integ target (30m). No per-test ctx — `runner.Run` doesn't
// accept one anyway, so a local context.WithTimeout would be dead
// code (verified slice 5.4 r3).
//
// Compose project cleanup: handled by the factory closure's
// t.Cleanup() in conformance_agent_claude_docker_test.go. bucket14c.go
// is non-_test.go and cannot import cleanupDockerOrphans (which lives
// in a //go:build integ _test.go file).
func testBucket14cGateUnderRealClaude(t *testing.T, factory AgentBackendFactory) {
	t.Helper()
	env := factory(t)
	if env.Spec.Compose == nil {
		t.Skip("Bucket 14c requires a compose-backed factory (the vulnerable-service sibling)")
	}

	// Resolve the workflow YAML path via runtime.Caller-anchored repo
	// root (set up in conformance/agent_test_helpers.go).
	repoRoot := conformanceRepoRoot()
	workflowPath := filepath.Join(repoRoot, "cli", "testdata", "phase5", "gate-repair-cve.yaml")

	// Real claude adapter wrapped in a Registry.
	var reg agent.Registry
	if rerr := reg.Register(env.Adapter); rerr != nil {
		t.Fatalf("Register: %v", rerr)
	}

	// awfRunID is the AWF run identifier — used for the log path
	// <stateDir>/runs/<awfRunID>/log. INTENTIONALLY distinct from the
	// factory's dockerRunID (which becomes the container-name prefix
	// `awf-<dockerRunID>-...` per container/docker/backend.go:179).
	// Two different IDs; same prefix-style name. See factory closure
	// in conformance_agent_claude_docker_test.go.
	awfRunID := fmt.Sprintf("bucket14c-%d", time.Now().UnixNano())
	stateDir := t.TempDir()

	runner := &cli.Runner{
		Backend:  env.Backend,
		Resolver: &reg,
		IDGen:    &clock.Fake{IDs: []string{awfRunID}},
	}

	var stdout, stderr bytes.Buffer
	rc := runner.Run(
		[]string{
			"run",
			"--state-dir", stateDir,
			"--backend", "docker",
			"--input", `{"target_url":"http://vulnerable:8080"}`,
			workflowPath,
		},
		&stdout, &stderr,
	)
	if rc != cli.ExitOK {
		t.Fatalf("runner.Run rc = %d; want %d\nstdout:\n%s\nstderr:\n%s",
			rc, cli.ExitOK, stdout.String(), stderr.String())
	}

	// Read the log + count gate.attempt events on gate[0].
	logPath := filepath.Join(stateDir, "runs", awfRunID, "log")
	events, ferr := state.FoldFile(logPath)
	if ferr != nil {
		t.Fatalf("state.FoldFile(%s): %v", logPath, ferr)
	}
	gateAttempts := 0
	for _, ev := range events {
		// state.Event field is Path (not NodePath) — verified at state/event.go:23-32.
		if ev.Type == engine.EventGateAttempt && ev.Path == "gate[0]" {
			gateAttempts++
		}
	}
	if gateAttempts < 1 {
		t.Errorf("gate.attempt events on gate[0] = %d; want >= 1 (proves the gate primitive ran end-to-end under real claude)", gateAttempts)
	}
}
