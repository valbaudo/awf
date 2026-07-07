package codexlive

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/live"
	"github.com/valbaudo/awf/container"
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

// TestCodexLiveForwardsReasoningDeltaAsLiveness locks D1: codex reasoning-summary
// deltas are the only signal codex emits while it is thinking. The drain loop must
// forward each one as a Live AgentEvent so awf's idle timer stays fed during
// reasoning instead of tripping a false stall.
func TestCodexLiveForwardsReasoningDeltaAsLiveness(t *testing.T) {
	root := testRoot(t)
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				{Type: EventReasoningSummaryDelta, Text: "planning the change"},
				{Type: EventTurnCompleted, Output: map[string]any{"ok": true}},
			},
		}},
	}
	a := newTestAdapter(t, root, fake)
	events, outcome := drainLaunchWithEvents(t, a, testInvocation(t.TempDir(), "reasoning"))
	if outcome.Err != nil {
		t.Fatalf("Launch outcome err: %v", outcome.Err)
	}
	var found bool
	for _, ev := range events {
		if ev.Kind == EventReasoningSummaryDelta {
			found = true
			if !ev.Live {
				t.Fatalf("reasoning-summary delta event Live = false: %+v", ev)
			}
			if !strings.Contains(string(ev.Payload), "planning the change") {
				t.Fatalf("reasoning delta payload missing text: %s", ev.Payload)
			}
		}
	}
	if !found {
		t.Fatalf("no %q liveness event emitted; events = %+v", EventReasoningSummaryDelta, events)
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

// TestCodexLiveContinueRetryResumesInsteadOfReplay (R5): on a continue-retry
// (inv.Attempt>0 AND inv.RecoveryContinue), prepareSession must NOT surface
// ErrLiveReplayRequired for a leftover ActiveTurn — even one at
// PhaseProviderTurnStarted (a mid-turn stall). It abandons that turn and resumes
// the thread with a fresh turn. Observed through Launch: fake.resumes==1 and the
// session record's ActiveTurn is cleared, whereas the same leftover WITHOUT the
// continue-retry signal still hard-halts with ErrLiveReplayRequired.
func TestCodexLiveContinueRetryResumesInsteadOfReplay(t *testing.T) {
	seed := func(t *testing.T, root live.Root, cwd, session string) {
		t.Helper()
		rec := live.SessionRecord{
			AdapterRef:             AdapterRef,
			SessionKey:             session,
			CWD:                    cwd,
			CanonicalCWD:           mustEvalSymlinks(t, cwd),
			ProviderSessionID:      "thread-1",
			ProviderBinary:         "/bin/codex",
			ProviderProtocolSchema: AppServerSchemaDigest(),
			ActiveTurn: &live.ActiveTurn{
				Phase:          live.PhaseProviderTurnStarted, // mid-turn stall left this behind
				RunID:          "run-1",
				NodePath:       "node-1",
				CurrentEpoch:   1,
				NextEpoch:      1,
				PromptDigest:   "sha256:prompt",
				LeaseID:        "lease-1",
				ProviderTurnID: "turn-1",
			},
		}
		if err := live.WriteSessionRecord(root, rec); err != nil {
			t.Fatalf("WriteSessionRecord: %v", err)
		}
	}

	t.Run("continue-retry resumes and clears the abandoned turn", func(t *testing.T) {
		root := testRoot(t)
		cwd := t.TempDir()
		fake := &fakeClient{
			info: ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
			turns: []fakeTurn{{
				turnID: "turn-2",
				events: []ProviderEvent{{Type: EventTurnCompleted, Output: map[string]any{"ok": true}}},
			}},
		}
		seed(t, root, cwd, "continue")
		a := newTestAdapter(t, root, fake)
		inv := testInvocation(cwd, "continue")
		inv.Attempt = 2             // R1 per-attempt signal: this is a retry
		inv.RecoveryContinue = true // resolved recovery == continue

		outcome := drainLaunch(t, a, inv)
		if outcome.Err != nil {
			t.Fatalf("Launch outcome err = %v, want nil (resumed, no replay)", outcome.Err)
		}
		if fake.resumes != 1 {
			t.Fatalf("resumes = %d, want 1 (continued the existing thread)", fake.resumes)
		}
		rec, err := live.ReadSessionRecord(root, AdapterRef, "continue")
		if err != nil {
			t.Fatalf("ReadSessionRecord: %v", err)
		}
		// After a successful resumed turn the record's ActiveTurn reflects the NEW
		// turn (turn-2), never the abandoned turn-1.
		if rec.ActiveTurn != nil && rec.ActiveTurn.ProviderTurnID == "turn-1" {
			t.Fatalf("ActiveTurn still references the abandoned turn-1: %+v", rec.ActiveTurn)
		}
	})

	t.Run("attempt>0 without recovery:continue still hard-halts", func(t *testing.T) {
		root := testRoot(t)
		cwd := t.TempDir()
		fake := &fakeClient{info: ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"}}
		seed(t, root, cwd, "restart")
		a := newTestAdapter(t, root, fake)
		inv := testInvocation(cwd, "restart")
		inv.Attempt = 2
		inv.RecoveryContinue = false // recovery == restart

		_, _, err := a.Launch(context.Background(), container.Handle{}, inv)
		if !errors.Is(err, agent.ErrLiveReplayRequired) {
			t.Fatalf("Launch err = %v, want ErrLiveReplayRequired (restart keeps cross-process replay)", err)
		}
		if fake.resumes != 0 {
			t.Fatalf("resumes = %d, want 0 (no resume on restart)", fake.resumes)
		}
	})
}
