package live_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/awf/agent/live"
)

func TestOpenRootCreatesPrivateDirectory(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	info, err := os.Stat(root.Path)
	if err != nil {
		t.Fatalf("Stat root: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("root mode = %o, want 0700", info.Mode().Perm())
	}
	if root.Pin.Path == "" || root.Pin.Digest == "" {
		t.Fatalf("root pin missing path/digest: %+v", root.Pin)
	}
}

func TestOpenRootDefaultsToStateDirLive(t *testing.T) {
	stateDir := t.TempDir()
	root, err := live.OpenRoot(stateDir, nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(stateDir, "live"))
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != want {
		t.Fatalf("root.Path = %q, want %q", root.Path, want)
	}
}

func TestRunStartedPinsLiveHomeOverrideAndResumeRejectsDrift(t *testing.T) {
	stateDir := t.TempDir()
	home := filepath.Join(t.TempDir(), "custom-live")
	root, err := live.OpenRoot(stateDir, map[string]string{"AWF_LIVE_HOME": home})
	if err != nil {
		t.Fatalf("OpenRoot override: %v", err)
	}
	wantHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != wantHome {
		t.Fatalf("root.Path = %q, want canonical override %q", root.Path, wantHome)
	}
	if err := live.CheckHomePin(root.Pin, stateDir, map[string]string{"AWF_LIVE_HOME": home}); err != nil {
		t.Fatalf("CheckHomePin same override: %v", err)
	}
	other := filepath.Join(t.TempDir(), "other-live")
	err = live.CheckHomePin(root.Pin, stateDir, map[string]string{"AWF_LIVE_HOME": other})
	if !errors.Is(err, live.ErrLiveHomeDrift) {
		t.Fatalf("CheckHomePin drift err = %v, want ErrLiveHomeDrift", err)
	}
}

func TestOpenRootRejectsWrongOwnerOrWritableRoot(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "live")
	if err := os.MkdirAll(rootPath, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rootPath, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := live.OpenRoot(t.TempDir(), map[string]string{"AWF_LIVE_HOME": rootPath})
	if !errors.Is(err, live.ErrUnsafeRoot) {
		t.Fatalf("OpenRoot writable root err = %v, want ErrUnsafeRoot", err)
	}
}

func TestOpenRootRejectsSymlinkComponent(t *testing.T) {
	parent := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := live.OpenRoot(t.TempDir(), map[string]string{"AWF_LIVE_HOME": filepath.Join(link, "live")})
	if !errors.Is(err, live.ErrUnsafeRoot) {
		t.Fatalf("OpenRoot symlink component err = %v, want ErrUnsafeRoot", err)
	}
}

func TestSessionKeyRejectsPathTraversal(t *testing.T) {
	for _, key := range []string{"", ".", "..", "../x", "x/y", `x\y`, "has space"} {
		if err := live.ValidateSessionKey(key); !errors.Is(err, live.ErrInvalidSessionKey) {
			t.Fatalf("ValidateSessionKey(%q) = %v, want ErrInvalidSessionKey", key, err)
		}
	}
	if err := live.ValidateSessionKey("awf-run-builder_1"); err != nil {
		t.Fatalf("ValidateSessionKey valid: %v", err)
	}
}

func TestWriteSessionRecordRejectsSymlinkParent(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root.Path, "sessions")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	err = live.WriteSessionRecord(root, live.SessionRecord{
		AdapterRef:   "openai/codex-live",
		SessionKey:   "builder",
		CanonicalCWD: t.TempDir(),
	})
	if !errors.Is(err, live.ErrUnsafeRoot) {
		t.Fatalf("WriteSessionRecord symlink parent err = %v, want ErrUnsafeRoot", err)
	}
}

func TestSessionRecordRejectsCanonicalCWDDrift(t *testing.T) {
	existing := live.SessionRecord{
		AdapterRef:             "openai/codex-live",
		SessionKey:             "builder",
		CanonicalCWD:           "/repo/a",
		ProviderBinary:         "/bin/codex",
		ProviderProtocolSchema: "schema-a",
	}
	next := existing
	next.CanonicalCWD = "/repo/b"
	err := live.CheckSessionDrift(existing, next)
	if !errors.Is(err, live.ErrSessionDrift) {
		t.Fatalf("CheckSessionDrift cwd err = %v, want ErrSessionDrift", err)
	}
}

func TestSessionRecordRejectsMissingSchemaDrift(t *testing.T) {
	existing := live.SessionRecord{
		AdapterRef:   "openai/codex-live",
		SessionKey:   "builder",
		CanonicalCWD: "/repo/a",
	}
	next := existing
	next.Schema = live.SessionSchema
	err := live.CheckSessionDrift(existing, next)
	if !errors.Is(err, live.ErrSessionDrift) {
		t.Fatalf("CheckSessionDrift missing schema err = %v, want ErrSessionDrift", err)
	}
}

func TestReadSessionRecordRejectsMissingSchema(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	path := live.SessionRecordPath(root, "openai/codex-live", "builder")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll session dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"adapter_ref":"openai/codex-live","session_key":"builder","canonical_cwd":"/repo"}`), 0o600); err != nil {
		t.Fatalf("WriteFile session: %v", err)
	}
	_, err = live.ReadSessionRecord(root, "openai/codex-live", "builder")
	if !errors.Is(err, live.ErrSessionDrift) {
		t.Fatalf("ReadSessionRecord missing schema err = %v, want ErrSessionDrift", err)
	}
}

func TestSessionRecordPersistsProviderMetadata(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	rec := live.SessionRecord{
		AdapterRef:             "openai/codex-live",
		SessionKey:             "builder",
		CWD:                    "/workspace",
		CanonicalCWD:           t.TempDir(),
		ProviderSessionID:      "provider-session-1",
		TmuxSession:            "tmux-name",
		TranscriptPath:         "/provider/transcript.jsonl",
		OwnerRunID:             "run-1",
		LastSeenUnix:           1_781_114_400,
		AdapterVersion:         "codex-live/1",
		ProviderBinary:         "/bin/codex",
		ProviderProtocolSchema: "schema-a",
	}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	got, err := live.ReadSessionRecord(root, rec.AdapterRef, rec.SessionKey)
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if got.CWD != rec.CWD || got.ProviderSessionID != rec.ProviderSessionID ||
		got.TmuxSession != rec.TmuxSession || got.TranscriptPath != rec.TranscriptPath ||
		got.OwnerRunID != rec.OwnerRunID || got.LastSeenUnix != rec.LastSeenUnix ||
		got.AdapterVersion != rec.AdapterVersion {
		t.Fatalf("metadata roundtrip = %+v, want %+v", got, rec)
	}
}

func TestSessionRecordRejectsProviderBinaryOrSchemaDrift(t *testing.T) {
	existing := live.SessionRecord{
		AdapterRef:             "openai/codex-live",
		SessionKey:             "builder",
		CanonicalCWD:           "/repo/a",
		ProviderBinary:         "/bin/codex",
		ProviderProtocolSchema: "schema-a",
	}
	next := existing
	next.ProviderBinary = "/other/codex"
	if err := live.CheckSessionDrift(existing, next); !errors.Is(err, live.ErrSessionDrift) {
		t.Fatalf("provider binary drift err = %v, want ErrSessionDrift", err)
	}
	next = existing
	next.ProviderProtocolSchema = "schema-b"
	if err := live.CheckSessionDrift(existing, next); !errors.Is(err, live.ErrSessionDrift) {
		t.Fatalf("provider schema drift err = %v, want ErrSessionDrift", err)
	}
}

func TestSessionRecordAtomicWrite(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	rec := live.SessionRecord{
		AdapterRef:             "openai/codex-live",
		SessionKey:             "builder",
		CanonicalCWD:           t.TempDir(),
		ProviderBinary:         "/bin/codex",
		ProviderProtocolSchema: "schema-a",
	}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	got, err := live.ReadSessionRecord(root, rec.AdapterRef, rec.SessionKey)
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if got.Schema != live.SessionSchema || got.AdapterRef != rec.AdapterRef || got.SessionKey != rec.SessionKey {
		t.Fatalf("record = %+v, want schema/ref/session filled", got)
	}
	info, err := os.Stat(live.SessionRecordPath(root, rec.AdapterRef, rec.SessionKey))
	if err != nil {
		t.Fatalf("Stat session record: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session mode = %o, want 0600", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(live.SessionRecordPath(root, rec.AdapterRef, rec.SessionKey)), "*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files left after atomic write: %v", matches)
	}
}

func TestTurnIntentRecordedAtomicWriteAndFsync(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	rec := live.SessionRecord{AdapterRef: "openai/codex-live", SessionKey: "builder", CanonicalCWD: t.TempDir()}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	turn := live.ActiveTurn{
		Phase:          live.PhaseIntentRecorded,
		RunID:          "r1",
		NodePath:       "build",
		CurrentEpoch:   1,
		NextEpoch:      2,
		PromptDigest:   "sha256:prompt",
		LeaseID:        "lease-1",
		ProviderTurnID: "",
	}
	if err := live.RecordTurnIntent(root, rec.AdapterRef, rec.SessionKey, turn); err != nil {
		t.Fatalf("RecordTurnIntent: %v", err)
	}
	got, err := live.ReadSessionRecord(root, rec.AdapterRef, rec.SessionKey)
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if got.ActiveTurn == nil || got.ActiveTurn.Phase != live.PhaseIntentRecorded || got.ActiveTurn.LeaseID != "lease-1" {
		t.Fatalf("ActiveTurn = %+v, want recorded intent", got.ActiveTurn)
	}
}

func TestActiveTurnPhaseClearRequiresNoProviderTurnID(t *testing.T) {
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	rec := live.SessionRecord{
		AdapterRef:   "openai/codex-live",
		SessionKey:   "builder",
		CanonicalCWD: t.TempDir(),
		ActiveTurn:   &live.ActiveTurn{Phase: live.PhaseIntentRecorded},
	}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	if err := live.ClearActiveTurnIfSafe(root, rec.AdapterRef, rec.SessionKey); err != nil {
		t.Fatalf("ClearActiveTurnIfSafe safe intent: %v", err)
	}
	got, _ := live.ReadSessionRecord(root, rec.AdapterRef, rec.SessionKey)
	if got.ActiveTurn != nil {
		t.Fatalf("ActiveTurn after clear = %+v, want nil", got.ActiveTurn)
	}

	rec.ActiveTurn = &live.ActiveTurn{Phase: live.PhaseIntentRecorded, ProviderTurnID: "provider-1"}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord provider turn: %v", err)
	}
	err = live.ClearActiveTurnIfSafe(root, rec.AdapterRef, rec.SessionKey)
	if !errors.Is(err, live.ErrActiveTurnNotClearable) {
		t.Fatalf("ClearActiveTurnIfSafe provider turn err = %v, want ErrActiveTurnNotClearable", err)
	}
}

// TestClearActiveTurnForRecoveryAbandonsProviderTurn (R4): the stall-recovery
// disposition clears an ActiveTurn even at PhaseProviderTurnStarted — the case
// ClearActiveTurnIfSafe refuses — so a continue-retry can resume the session.
// It is a no-op when there is no ActiveTurn.
func TestClearActiveTurnForRecoveryAbandonsProviderTurn(t *testing.T) {
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
			ProviderTurnID: "provider-1",
		},
	}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	// ClearActiveTurnIfSafe must still refuse this phase (guard unchanged).
	if err := live.ClearActiveTurnIfSafe(root, rec.AdapterRef, rec.SessionKey); !errors.Is(err, live.ErrActiveTurnNotClearable) {
		t.Fatalf("ClearActiveTurnIfSafe err = %v, want ErrActiveTurnNotClearable", err)
	}
	// ClearActiveTurnForRecovery abandons it.
	if err := live.ClearActiveTurnForRecovery(root, rec.AdapterRef, rec.SessionKey); err != nil {
		t.Fatalf("ClearActiveTurnForRecovery: %v", err)
	}
	got, err := live.ReadSessionRecord(root, rec.AdapterRef, rec.SessionKey)
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if got.ActiveTurn != nil {
		t.Fatalf("ActiveTurn after recovery clear = %+v, want nil", got.ActiveTurn)
	}
	// Idempotent no-op when already clear.
	if err := live.ClearActiveTurnForRecovery(root, rec.AdapterRef, rec.SessionKey); err != nil {
		t.Fatalf("ClearActiveTurnForRecovery no-op: %v", err)
	}
}
