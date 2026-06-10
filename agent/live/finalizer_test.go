package live_test

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent/live"
)

func TestLiveFinalizerReconcilesCommittedTurnFromRecord(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	rec := live.SessionRecord{
		AdapterRef:   "openai/codex-live",
		SessionKey:   "builder",
		CanonicalCWD: t.TempDir(),
		ActiveTurn: &live.ActiveTurn{
			Phase:          live.PhaseProviderTurnStarted,
			RunID:          "run-1",
			NodePath:       "build",
			CurrentEpoch:   1,
			NextEpoch:      2,
			PromptDigest:   "sha256:prompt",
			LeaseID:        "lease-1",
			ProviderTurnID: "provider-turn-1",
		},
	}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	turn := live.CommittedTurn{
		RunID:          "run-1",
		NodePath:       "build",
		Epoch:          2,
		ProviderTurnID: "provider-turn-1",
		CommittedUnix:  1_781_114_500,
	}
	if err := live.FinalizeCommittedTurn(root, rec.AdapterRef, rec.SessionKey, turn); err != nil {
		t.Fatalf("FinalizeCommittedTurn: %v", err)
	}
	got, err := live.ReadSessionRecord(root, rec.AdapterRef, rec.SessionKey)
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if got.ActiveTurn != nil {
		t.Fatalf("ActiveTurn = %+v, want cleared", got.ActiveTurn)
	}
	if got.LastCommittedTurn == nil || *got.LastCommittedTurn != turn {
		t.Fatalf("LastCommittedTurn = %+v, want %+v", got.LastCommittedTurn, turn)
	}
}

func TestLiveFinalizerRejectsMismatchedActiveTurn(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	rec := live.SessionRecord{
		AdapterRef:   "openai/codex-live",
		SessionKey:   "builder",
		CanonicalCWD: t.TempDir(),
		ActiveTurn: &live.ActiveTurn{
			Phase:          live.PhaseProviderTurnStarted,
			RunID:          "run-2",
			NodePath:       "other",
			NextEpoch:      3,
			ProviderTurnID: "provider-turn-2",
		},
	}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	err = live.FinalizeCommittedTurn(root, rec.AdapterRef, rec.SessionKey, live.CommittedTurn{
		RunID:          "run-1",
		NodePath:       "build",
		Epoch:          2,
		ProviderTurnID: "provider-turn-1",
		CommittedUnix:  1_781_114_500,
	})
	if !errors.Is(err, live.ErrActiveTurnMismatch) {
		t.Fatalf("FinalizeCommittedTurn mismatch err = %v, want ErrActiveTurnMismatch", err)
	}
	got, err := live.ReadSessionRecord(root, rec.AdapterRef, rec.SessionKey)
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if got.ActiveTurn == nil || got.ActiveTurn.RunID != "run-2" {
		t.Fatalf("ActiveTurn after mismatch = %+v, want original preserved", got.ActiveTurn)
	}
}

func TestLiveFinalizerRejectsEmptyProviderTurnForActiveProviderTurn(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	rec := live.SessionRecord{
		AdapterRef:   "openai/codex-live",
		SessionKey:   "builder",
		CanonicalCWD: t.TempDir(),
		ActiveTurn: &live.ActiveTurn{
			Phase:          live.PhaseProviderTurnStarted,
			RunID:          "run-1",
			NodePath:       "build",
			NextEpoch:      2,
			ProviderTurnID: "provider-turn-1",
		},
	}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	err = live.FinalizeCommittedTurn(root, rec.AdapterRef, rec.SessionKey, live.CommittedTurn{
		RunID:         "run-1",
		NodePath:      "build",
		Epoch:         2,
		CommittedUnix: 1_781_114_500,
	})
	if !errors.Is(err, live.ErrActiveTurnMismatch) {
		t.Fatalf("FinalizeCommittedTurn empty provider turn err = %v, want ErrActiveTurnMismatch", err)
	}
}
