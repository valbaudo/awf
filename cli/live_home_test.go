package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	agentfake "github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/agent/live"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/state"
)

func TestCLIRunRecordsLiveHomePin(t *testing.T) {
	stateDir := t.TempDir()
	home := filepath.Join(t.TempDir(), "custom-live")
	t.Setenv("AWF_LIVE_HOME", home)

	var reg agent.Registry
	if err := reg.Register(agentfake.New("test/fake").Script(0, agentfake.Result{Output: map[string]any{"ok": true}})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	runner := &Runner{
		Backend:  container.NewFake(),
		Resolver: &reg,
		IDGen:    &clock.Fake{IDs: []string{"live-home-run"}},
	}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"run", "--state-dir", stateDir, "testdata/render-probe.yaml"}, &stdout, &stderr)
	if rc != ExitOK {
		t.Fatalf("rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}

	started := readRunStartedForTest(t, stateDir, "live-home-run")
	wantPath, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("EvalSymlinks(home): %v", err)
	}
	if started.LiveHome == nil || started.LiveHome.Path != wantPath || started.LiveHome.Digest == "" {
		t.Fatalf("LiveHome = %+v, want path %q and digest", started.LiveHome, wantPath)
	}
}

func TestCLIResumeRejectsLiveHomeDriftBeforeRunResumed(t *testing.T) {
	stateDir := t.TempDir()
	runID := "live-home-drift"
	homeA := filepath.Join(t.TempDir(), "live-a")
	root, err := live.OpenRoot(stateDir, map[string]string{"AWF_LIVE_HOME": homeA})
	if err != nil {
		t.Fatalf("OpenRoot homeA: %v", err)
	}
	ld, err := loader.Load("testdata/render-probe.yaml")
	if err != nil {
		t.Fatalf("Load workflow: %v", err)
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	runDir := filepath.Join(stateDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll runDir: %v", err)
	}
	if _, err := state.OpenBlobs(filepath.Join(stateDir, "blobs")); err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	log, err := state.OpenLogExclusive(filepath.Join(runDir, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLogExclusive: %v", err)
	}
	startedData, err := json.Marshal(engine.RunStartedData{
		RunID:          runID,
		WorkflowDigest: digest,
		Backend:        engine.BackendFake,
		LiveHome:       &engine.LiveHomePin{Path: root.Pin.Path, Digest: root.Pin.Digest},
	})
	if err != nil {
		t.Fatalf("Marshal run.started: %v", err)
	}
	if err := log.Append(state.Event{Type: engine.EventRunStarted, Data: startedData}); err != nil {
		t.Fatalf("Append run.started: %v", err)
	}
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync log: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close log: %v", err)
	}

	t.Setenv("AWF_LIVE_HOME", filepath.Join(t.TempDir(), "live-b"))
	runner := &Runner{Backend: container.NewFake(), IDGen: &clock.Fake{}}
	var stdout, stderr bytes.Buffer
	rc := runner.Run([]string{"resume", "--state-dir", stateDir, runID, "testdata/render-probe.yaml"}, &stdout, &stderr)
	if rc != ExitUsage {
		t.Fatalf("rc = %d, want ExitUsage; stdout=%q stderr=%q", rc, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "live home drift") {
		t.Fatalf("stderr = %q, want live home drift", stderr.String())
	}
	events := readRunEventsForTest(t, stateDir, runID)
	for _, ev := range events {
		if ev.Type == engine.EventRunResumed {
			t.Fatalf("run.resumed was appended despite live home drift: %+v", ev)
		}
	}
}

func TestCodexLiveReleasesLeaseAfterPostCommitFinalizer(t *testing.T) {
	stateDir := t.TempDir()
	root, err := live.OpenRoot(stateDir, nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	cwd := t.TempDir()
	rec := live.SessionRecord{
		AdapterRef:   "openai/codex-live",
		SessionKey:   "builder",
		CanonicalCWD: cwd,
		ActiveTurn: &live.ActiveTurn{
			Phase:          live.PhaseProviderTurnStarted,
			RunID:          "run-1",
			NodePath:       "build",
			CurrentEpoch:   1,
			NextEpoch:      2,
			PromptDigest:   "sha256:prompt",
			LeaseID:        live.LeaseID("run-1", 2, "build", "builder"),
			ProviderTurnID: "provider-turn-1",
		},
	}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	leaseID := rec.ActiveTurn.LeaseID
	if _, err := live.AcquireLease(root, live.LeaseRequest{
		AdapterRef:    rec.AdapterRef,
		SessionKey:    rec.SessionKey,
		OwnerRunID:    rec.ActiveTurn.RunID,
		OwnerPID:      os.Getpid(),
		OwnerNodePath: rec.ActiveTurn.NodePath,
		OwnerEpoch:    rec.ActiveTurn.NextEpoch,
		LeaseID:       leaseID,
		TTLSeconds:    120,
	}, clock.System{}.Now(), nil); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	finalize := liveDispatchFinalizer(root)
	err = finalize(context.Background(), engine.LiveDispatchRecord{
		AdapterRef:     rec.AdapterRef,
		SessionKey:     rec.SessionKey,
		SessionKeyHash: "sha256:session",
		LeaseID:        leaseID,
		ActiveTurnID:   "intent",
		ProviderTurnID: rec.ActiveTurn.ProviderTurnID,
		RunID:          rec.ActiveTurn.RunID,
		NodePath:       rec.ActiveTurn.NodePath,
		Epoch:          uint32(rec.ActiveTurn.NextEpoch),
		CommittedUnix:  1_781_114_500,
	})
	if err != nil {
		t.Fatalf("liveDispatchFinalizer: %v", err)
	}
	got, err := live.ReadSessionRecord(root, rec.AdapterRef, rec.SessionKey)
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if got.ActiveTurn != nil {
		t.Fatalf("ActiveTurn = %+v, want cleared", got.ActiveTurn)
	}
	if got.LastCommittedTurn == nil || got.LastCommittedTurn.ProviderTurnID != "provider-turn-1" {
		t.Fatalf("LastCommittedTurn = %+v, want provider turn recorded", got.LastCommittedTurn)
	}
	_, err = live.AcquireLease(root, live.LeaseRequest{
		AdapterRef:    rec.AdapterRef,
		SessionKey:    rec.SessionKey,
		OwnerRunID:    "run-2",
		OwnerPID:      os.Getpid(),
		OwnerNodePath: "build",
		OwnerEpoch:    3,
		LeaseID:       live.LeaseID("run-2", 3, "build", "builder"),
		TTLSeconds:    120,
	}, clock.System{}.Now(), nil)
	if err != nil {
		t.Fatalf("AcquireLease after finalizer release: %v", err)
	}
}

func readRunStartedForTest(t *testing.T, stateDir, runID string) engine.RunStartedData {
	t.Helper()
	events := readRunEventsForTest(t, stateDir, runID)
	started, err := engine.RunStartedDataFromEvents(events)
	if err != nil {
		t.Fatalf("RunStartedDataFromEvents: %v", err)
	}
	return started
}

func readRunEventsForTest(t *testing.T, stateDir, runID string) []state.Event {
	t.Helper()
	log, err := state.OpenLog(filepath.Join(stateDir, "runs", runID, "log"), clock.System{})
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer func() { _ = log.Close() }()
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	return events
}
