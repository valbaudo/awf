package codexlive

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/ir"
)

// TestCodexLiveLaunchPrependsGateFeedback locks F31a: codexlive is the ONLY
// gate-repair-capable adapter that used to send cfg.prompt to the provider
// (and hash it into the replay PromptDigest) WITHOUT the gate's prior verdict
// prepended. That silently degraded repairs into blind identical retries.
// The seam this test observes is genuinely load-bearing: fakeClient.StartTurn
// records every TurnStartRequest it receives (see adapter_test.go), so
// asserting on fake.turnRequests[0].Prompt observes exactly what codexlive
// would hand the app-server over JSON-RPC — not a mock rigged to pass.
func TestCodexLiveLaunchPrependsGateFeedback(t *testing.T) {
	root := testRoot(t)
	cwd := t.TempDir()
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{{Type: EventTurnCompleted, Output: map[string]any{"ok": true}}},
		}},
	}
	a := newTestAdapter(t, root, fake)
	inv := testInvocation(cwd, "feedback")
	inv.Feedback = ir.RawConfig{"passed": false, "reason": "missing citation"}

	outcome := drainLaunch(t, a, inv)
	if outcome.Err != nil {
		t.Fatalf("Launch outcome err: %v", outcome.Err)
	}
	if len(fake.turnRequests) != 1 {
		t.Fatalf("turn requests = %d, want 1", len(fake.turnRequests))
	}

	got := fake.turnRequests[0].Prompt
	if !strings.HasPrefix(got, "<previous verdict>\n") {
		t.Fatalf("codexlive did not prepend gate feedback; sent prompt = %q", got)
	}
	if !strings.Contains(got, "missing citation") {
		t.Fatalf("feedback content missing from sent prompt: %q", got)
	}
	if !strings.HasSuffix(got, "\n\nplease build") {
		t.Fatalf("original prompt (%q) not preserved as suffix of sent prompt: %q", "please build", got)
	}
}

// TestCodexLiveLaunchNoFeedbackLeavesPromptUnchanged locks the empty-feedback
// no-op path (attempt 1, or any invocation carrying no gate verdict): the
// prompt sent to the provider must be byte-identical to the raw input, same
// as before this fix.
func TestCodexLiveLaunchNoFeedbackLeavesPromptUnchanged(t *testing.T) {
	root := testRoot(t)
	cwd := t.TempDir()
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{{Type: EventTurnCompleted, Output: map[string]any{"ok": true}}},
		}},
	}
	a := newTestAdapter(t, root, fake)
	inv := testInvocation(cwd, "no-feedback")

	outcome := drainLaunch(t, a, inv)
	if outcome.Err != nil {
		t.Fatalf("Launch outcome err: %v", outcome.Err)
	}
	if len(fake.turnRequests) != 1 {
		t.Fatalf("turn requests = %d, want 1", len(fake.turnRequests))
	}
	if got, want := fake.turnRequests[0].Prompt, "please build"; got != want {
		t.Fatalf("sent prompt = %q, want unchanged %q", got, want)
	}
}
