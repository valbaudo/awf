//go:build integ && live

package claudesession_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claudesession"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/container/native"
	"github.com/valbaudo/awf/ir"
)

// skipIfNoClaude skips when the claude binary is absent — mirrors the
// predicate in agent/claude/version_integ_test.go verbatim.
func skipIfNoClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH; install Claude Code or unset --tags=live for this test")
	}
}

// skipIfNoAuthEnv skips when neither ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN,
// nor CLAUDE_CODE_OAUTH_TOKEN is set — mirrors the predicate in
// agent/claude/version_integ_test.go verbatim.
func skipIfNoAuthEnv(t *testing.T) {
	t.Helper()
	for _, name := range claudesession.DefaultEnvAllowlist {
		if os.Getenv(name) != "" {
			return
		}
	}
	t.Skip("no auth env (ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN / CLAUDE_CODE_OAUTH_TOKEN); skipping live integ test")
}

// hostEnvMap builds the auth env map from variables set on the host.
func hostEnvMap() map[string]string {
	out := map[string]string{}
	for _, name := range claudesession.DefaultEnvAllowlist {
		if v := os.Getenv(name); v != "" {
			out[name] = v
		}
	}
	return out
}

// newSessionAdapterForInteg constructs a claudesession.Adapter backed by the
// native backend, using os.UserHomeDir() as the homeDir so that
// SessionTranscriptPath matches where claude actually writes on this host.
func newSessionAdapterForInteg(t *testing.T) (*claudesession.Adapter, container.Handle, container.Backend) {
	t.Helper()
	be, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	h, err := be.Create(context.Background(), container.ContainerSpec{Name: "session-lab"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = be.Destroy(context.Background(), h) })

	// homeDir MUST match the host's actual home so SessionTranscriptPath
	// returns the same path that claude writes the transcript to.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}

	a, err := claudesession.New(
		claudesession.WithBackend(be),
		claudesession.WithEnv(hostEnvMap()),
		claudesession.WithHomeDir(homeDir),
	)
	if err != nil {
		t.Fatalf("claudesession.New: %v", err)
	}
	return a, h, be
}

// drainAndWait drains the event channel then reads the single outcome.
func drainAndWait(eventCh <-chan agent.AgentEvent, outcomeCh <-chan agent.AgentOutcome) agent.AgentOutcome {
	for range eventCh {
	}
	return <-outcomeCh
}

// TestClaudeSessionTranscriptPathCapture is the highest-value assertion:
// run a real `claude-code-session` turn and verify that the transcript file
// appears at EXACTLY the path that SessionTranscriptPath() derives.
//
// This pins the path-derivation contract (homeDir / encodeProjectDir(workdir) /
// sessionUUID.jsonl) against the claude binary's actual journal location.
// A mismatch here means AWF would capture/restore the wrong file on the
// cve-runner (or any other deployment).
func TestClaudeSessionTranscriptPathCapture(t *testing.T) {
	skipIfNoClaude(t)
	skipIfNoAuthEnv(t)

	a, h, _ := newSessionAdapterForInteg(t)

	// Use the native backend workdir as the step workdir — on the native
	// backend, claude's cwd is h.ID (the handle workdir path). We pass that
	// as with.workdir so encodeProjectDir encodes the right directory.
	workdir := h.ID // absolute host path: <tmpdir>/session-lab

	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     claudesession.AdapterRef,
		RunContext: agent.RunContext{
			RunID:        "live-integ-capture-" + time.Now().Format("20060102T150405"),
			CurrentEpoch: 1,
		},
		With: ir.RawConfig{
			"prompt":  "Reply with a JSON object containing only the key 'ok' set to true.",
			"workdir": workdir,
		},
		OutputSchema: &ir.JSONSchema{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"ok"},
			"properties": map[string]any{
				"ok": map[string]any{"type": "boolean"},
			},
		},
	}

	expectedPath := a.SessionTranscriptPath(inv, workdir)
	t.Logf("expected transcript path: %s", expectedPath)

	// Remove any pre-existing transcript file so the assertion is
	// unambiguous (a stale file from a prior run must not fool the test).
	_ = os.Remove(expectedPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	eventCh, outcomeCh, err := a.Launch(ctx, h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	outcome := drainAndWait(eventCh, outcomeCh)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if v, ok := outcome.Result.Output["ok"].(bool); !ok || !v {
		t.Errorf("Output[ok] = %v (%T), want true", outcome.Result.Output["ok"], outcome.Result.Output["ok"])
	}

	// PRIMARY ASSERTION: the transcript file must exist at exactly the
	// path SessionTranscriptPath() derived — this proves the path formula
	// matches where claude actually writes on this host.
	if _, statErr := os.Stat(expectedPath); statErr != nil {
		t.Fatalf("transcript file not found at derived path %q: %v\n"+
			"homeDir=%q workdir=%q\n"+
			"This means SessionTranscriptPath encodes the wrong location — "+
			"capture/restore will silently miss the transcript on the cve-runner.",
			expectedPath, statErr,
			mustUserHomeDir(t), workdir)
	}
	t.Logf("transcript exists at expected path (capture-path assertion PASS)")

	// Read and sanity-check the transcript content.
	data, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("ReadFile transcript: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("transcript file is empty")
	}
	// The transcript is JSONL; each line is a JSON object. A minimal check:
	// at least one line must be valid JSON (non-empty, starts with '{').
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		t.Fatal("transcript has no lines")
	}
	found := false
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "{") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("transcript does not contain any JSON-object lines; first 200 bytes: %q", data[:min(200, len(data))])
	}
	t.Logf("transcript sanity-check PASS (%d bytes, %d lines)", len(data), len(lines))
}

// TestClaudeSessionRestoreAndResume validates the full capture→restore→resume
// round-trip:
//  1. Turn 1: ask claude to remember a secret word ("banana").
//  2. Capture the transcript file from the host filesystem.
//  3. Simulate engine restore: delete the transcript, write it back via
//     os.WriteFile (the same thing the engine does via state.Blobs + WriteFileAt,
//     but exercised here at the host-fs level to keep the test self-contained).
//  4. Turn 2: with ResumeSession=true (adapter passes --resume <uuid>), ask
//     "what word did I ask you to remember?" — assert the response contains "banana".
func TestClaudeSessionRestoreAndResume(t *testing.T) {
	skipIfNoClaude(t)
	skipIfNoAuthEnv(t)

	a, h, _ := newSessionAdapterForInteg(t)
	workdir := h.ID

	runID := "live-integ-resume-" + time.Now().Format("20060102T150405")

	baseInv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     claudesession.AdapterRef,
		RunContext: agent.RunContext{
			RunID:        runID,
			CurrentEpoch: 1,
		},
		With: ir.RawConfig{
			"workdir": workdir,
		},
		// No OutputSchema — prose response for the recall turn.
	}

	transcriptPath := a.SessionTranscriptPath(baseInv, workdir)
	t.Logf("session transcript path: %s", transcriptPath)

	// Pre-clean any leftover from a prior run.
	_ = os.Remove(transcriptPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// ---- Turn 1: establish the session, plant the secret word ----

	inv1 := baseInv
	inv1.With = ir.RawConfig{
		"prompt":  "Please remember the secret word 'banana'. Acknowledge by saying you have noted it.",
		"workdir": workdir,
	}

	eventCh1, outcomeCh1, err := a.Launch(ctx, h, inv1)
	if err != nil {
		t.Fatalf("Turn 1 Launch: %v", err)
	}
	out1 := drainAndWait(eventCh1, outcomeCh1)
	if out1.Err != nil {
		t.Fatalf("Turn 1 outcome.Err = %v", out1.Err)
	}
	t.Logf("Turn 1 complete")

	// Verify the transcript exists.
	rawTranscript, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("cannot read transcript after Turn 1 at %q: %v", transcriptPath, err)
	}
	if len(rawTranscript) == 0 {
		t.Fatal("transcript empty after Turn 1")
	}
	t.Logf("transcript captured: %d bytes", len(rawTranscript))

	// Simulate engine restore: delete the transcript, then write it back.
	// This proves the restore path (not just continuity within a live process).
	if err := os.Remove(transcriptPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove transcript: %v", err)
	}
	// Re-create parent directories in case Remove left a gap (it won't, but
	// be defensive so the test is robust).
	if err := os.MkdirAll(transcriptPath[:strings.LastIndex(transcriptPath, "/")], 0o755); err != nil {
		t.Fatalf("MkdirAll transcript dir: %v", err)
	}
	if err := os.WriteFile(transcriptPath, rawTranscript, 0o644); err != nil {
		t.Fatalf("WriteFile transcript (restore): %v", err)
	}
	t.Logf("transcript restored to %s", transcriptPath)

	// ---- Turn 2: ResumeSession=true; assert recall of the secret word ----

	inv2 := baseInv
	inv2.ResumeSession = true
	inv2.With = ir.RawConfig{
		"prompt":  "What secret word did I ask you to remember in our previous conversation? Reply with just that word.",
		"workdir": workdir,
	}

	eventCh2, outcomeCh2, err := a.Launch(ctx, h, inv2)
	if err != nil {
		t.Fatalf("Turn 2 Launch: %v", err)
	}

	// Collect all assistant text from events for the recall assertion.
	var assistantText strings.Builder
	for ev := range eventCh2 {
		if ev.Display.Text != "" {
			assistantText.WriteString(ev.Display.Text)
			assistantText.WriteString(" ")
		}
	}
	out2 := <-outcomeCh2
	if out2.Err != nil {
		t.Fatalf("Turn 2 outcome.Err = %v", out2.Err)
	}
	t.Logf("Turn 2 complete; assistant text: %q", assistantText.String())

	// RECALL ASSERTION: the response must contain the planted secret word.
	// Case-insensitive since the model may capitalise it.
	combined := strings.ToLower(assistantText.String())
	if !strings.Contains(combined, "banana") {
		t.Errorf("Turn 2 response does not contain 'banana' — session was NOT resumed correctly.\n"+
			"full assistant text: %q\n"+
			"transcript path: %q\n"+
			"This proves --resume <uuid> is either not passed or the transcript path is wrong.",
			assistantText.String(), transcriptPath)
	} else {
		t.Logf("RECALL ASSERTION PASS: response contains 'banana'")
	}
}

func mustUserHomeDir(t *testing.T) string {
	t.Helper()
	h, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	return h
}
