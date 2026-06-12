package codexlive

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/live"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/pricing"
)

func TestCodexLiveStartsAndResumesThread(t *testing.T) {
	root := testRoot(t)
	cwd := t.TempDir()
	fake := &fakeClient{
		info: ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{
			ID:             "thread-1",
			TmuxSession:    "tmux-1",
			TranscriptPath: "/tmp/transcript.jsonl",
		},
		turns: []fakeTurn{
			{turnID: "turn-1", events: []ProviderEvent{{Type: EventTurnCompleted, Output: map[string]any{"ok": true}}}},
			{turnID: "turn-2", events: []ProviderEvent{{Type: EventTurnCompleted, Output: map[string]any{"ok": true}}}},
		},
	}
	a := newTestAdapter(t, root, fake)

	inv := testInvocation(cwd, "builder")
	outcome := drainLaunch(t, a, inv)
	if outcome.Result.Live == nil {
		t.Fatal("AgentResult.Live after first launch = nil")
	}
	if err := finalizeLiveForTest(root, outcome.Result.Live, 101); err != nil {
		t.Fatalf("FinalizeCommittedTurn first launch: %v", err)
	}
	if fake.starts != 1 {
		t.Fatalf("starts = %d, want 1", fake.starts)
	}
	if fake.resumes != 0 {
		t.Fatalf("resumes after first launch = %d, want 0", fake.resumes)
	}
	rec, err := live.ReadSessionRecord(root, AdapterRef, "builder")
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if rec.ProviderSessionID != "thread-1" || rec.TmuxSession != "tmux-1" || rec.TranscriptPath != "/tmp/transcript.jsonl" {
		t.Fatalf("session record provider metadata = %+v", rec)
	}

	inv.RunContext.CurrentEpoch = 2
	inv.RunContext.NextEpoch = 2
	drainLaunch(t, a, inv)
	if fake.starts != 1 {
		t.Fatalf("starts after resume = %d, want 1", fake.starts)
	}
	if fake.resumes != 1 {
		t.Fatalf("resumes = %d, want 1", fake.resumes)
	}
	if got := fake.resumeThreadIDs[0]; got != "thread-1" {
		t.Fatalf("resume thread id = %q, want thread-1", got)
	}
}

func TestCodexLiveCapturesTokenUsageMetrics(t *testing.T) {
	root := testRoot(t)
	cwd := t.TempDir()
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				// usage arrives on its own notification, before turn/completed
				{Type: EventThreadTokenUsage, Usage: Usage{InputTokens: 1200, OutputTokens: 340, CachedInputTokens: 800}},
				{Type: EventTurnCompleted, Output: map[string]any{"ok": true}},
			},
		}},
	}
	a := newTestAdapter(t, root, fake)
	inv := testInvocation(cwd, "metered")
	outcome := drainLaunch(t, a, inv)
	got := outcome.Result.Metrics.Tokens
	if got.Input != 1200 || got.Output != 340 || got.CacheReadInput != 800 {
		t.Fatalf("Metrics.Tokens = %+v, want Input=1200 Output=340 CacheReadInput=800", got)
	}
}

func TestCodexLiveDerivesCostFromResolvedModelAndInclusionTotals(t *testing.T) {
	root := testRoot(t)
	cwd := t.TempDir()
	// thread/start surfaces the RESOLVED model; the token-usage event carries the
	// INCLUSION totals (1200+340 == 1540 → cached 800 ⊂ input → subtract).
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1", Model: "gpt-5.3-codex"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				{Type: EventThreadTokenUsage, Usage: Usage{InputTokens: 1200, OutputTokens: 340, CachedInputTokens: 800, TotalTokens: 1540}},
				{Type: EventTurnCompleted, Output: map[string]any{"ok": true}},
			},
		}},
	}
	// Self-contained fixture rates: input $2/M, output $10/M, cache-read $0.5/M.
	table := pricing.Table{"gpt-5.3-codex": {Currency: "USD", InputPerM: 2, OutputPerM: 10, CacheReadPerM: 0.5}}
	a, err := New(WithLiveRoot(root), WithClient(fake), WithClock(&clock.Fake{T: time.Unix(100, 0).UTC()}), WithPricing(table))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	inv := testInvocation(cwd, "priced") // note: with-config carries NO model
	outcome := drainLaunch(t, a, inv)

	ms := outcome.Result.Metrics
	if ms.Model != "gpt-5.3-codex" {
		t.Fatalf("Metrics.Model = %q, want gpt-5.3-codex (resolved from thread/start)", ms.Model)
	}
	c := ms.Cost
	if c.Source != agent.CostSourceDerived {
		t.Fatalf("Cost.Source = %q, want %q", c.Source, agent.CostSourceDerived)
	}
	if c.Currency != "USD" {
		t.Fatalf("Cost.Currency = %q, want USD", c.Currency)
	}
	// uncached input 400 tok·$2/M = $0.0008; cached 800·$0.5/M = $0.0004 → Input $0.0012.
	if !approxUSD(c.Input, 0.0012) {
		t.Fatalf("Cost.Input = %v, want 0.0012 (400·$2/M + 800·$0.5/M)", c.Input)
	}
	// output 340·$10/M = $0.0034.
	if !approxUSD(c.Output, 0.0034) {
		t.Fatalf("Cost.Output = %v, want 0.0034", c.Output)
	}
	if c.Total != c.Input+c.Output {
		t.Fatalf("Total %v != Input+Output %v (exact invariant)", c.Total, c.Input+c.Output)
	}
}

func approxUSD(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-12
}

func TestCodexLiveUsesNativeOutputSchema(t *testing.T) {
	root := testRoot(t)
	cwd := t.TempDir()
	schema := &ir.JSONSchema{
		"type":     "object",
		"required": []any{"ok"},
		"properties": map[string]any{
			"ok": map[string]any{"type": "boolean"},
		},
		"additionalProperties": false,
	}
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				{Type: EventItemCompleted, Text: `{"ok":true}`},
				{Type: EventTurnCompleted},
			},
		}},
	}
	a := newTestAdapter(t, root, fake)
	inv := testInvocation(cwd, "schema")
	inv.OutputSchema = schema
	outcome := drainLaunch(t, a, inv)
	if got := outcome.Result.Output["ok"]; got != true {
		t.Fatalf("output ok = %#v, want true", got)
	}
	if fake.turnRequests[0].OutputSchema == nil || !reflect.DeepEqual(*fake.turnRequests[0].OutputSchema, *schema) {
		t.Fatalf("turn output schema = %#v, want %#v", fake.turnRequests[0].OutputSchema, schema)
	}
}

func TestCodexLiveInvalidFinalJSONAfterProviderTurnRequiresReplay(t *testing.T) {
	root := testRoot(t)
	schema := &ir.JSONSchema{
		"type":                 "object",
		"required":             []any{"ok"},
		"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
		"additionalProperties": false,
	}
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				{Type: EventItemCompleted, Text: `not-json`},
				{Type: EventTurnCompleted, Status: "completed"},
			},
		}},
	}
	a := newTestAdapter(t, root, fake)
	inv := testInvocation(t.TempDir(), "bad-json")
	inv.OutputSchema = schema
	outcome := drainLaunch(t, a, inv)
	if !errors.Is(outcome.Err, agent.ErrLiveReplayRequired) {
		t.Fatalf("outcome err = %v, want ErrLiveReplayRequired", outcome.Err)
	}
	rec, err := live.ReadSessionRecord(root, AdapterRef, "bad-json")
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if rec.ActiveTurn == nil || rec.ActiveTurn.ProviderTurnID != "turn-1" {
		t.Fatalf("ActiveTurn = %+v, want provider turn retained for replay", rec.ActiveTurn)
	}
}

func TestCodexLiveVersionIncludesAppServerSchemaDigest(t *testing.T) {
	a := newVersionTestAdapter(t, &fakeClient{info: ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"}})
	got, err := a.Version(context.Background(), container.Handle{})
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	for _, want := range []string{"codex-cli/0.137.0", "protocol-schema/sha256:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Version = %q, want to contain %q", got, want)
		}
	}
	if got == live.FormatVersion("codex-cli/0.137.0", "") {
		t.Fatalf("Version = %q, want app-server schema digest included", got)
	}
}

func TestCodexLiveNewDefaultsToProcessClientForVersion(t *testing.T) {
	binDir := t.TempDir()
	codexPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codexPath, []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 9.9.9'; exit 0; fi\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := a.Version(context.Background(), container.Handle{})
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !strings.Contains(got, "codex-cli 9.9.9") || !strings.Contains(got, "protocol-schema/sha256:") {
		t.Fatalf("Version = %q, want fake codex version plus protocol digest", got)
	}
}

func TestCodexLiveBackpressureRetriesBeforeTurnStart(t *testing.T) {
	root := testRoot(t)
	fakeClock := &recordingClock{Clock: &clock.Fake{T: time.Unix(100, 0).UTC()}}
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{
			{err: &RPCError{Code: -32001, Message: "busy"}},
			{err: &RPCError{Code: -32001, Message: "busy"}},
			{turnID: "turn-1", events: []ProviderEvent{{Type: EventTurnCompleted, Output: map[string]any{"ok": true}}}},
		},
	}
	a, err := New(WithLiveRoot(root), WithClient(fake), WithClock(fakeClock), WithBackoff([]time.Duration{10 * time.Millisecond, 25 * time.Millisecond}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	drainLaunch(t, a, testInvocation(t.TempDir(), "busy"))
	if len(fake.turnRequests) != 3 {
		t.Fatalf("turn starts = %d, want 3", len(fake.turnRequests))
	}
	if got, want := fakeClock.sleeps, []time.Duration{10 * time.Millisecond, 25 * time.Millisecond}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sleeps = %v, want %v", got, want)
	}
}

func TestCodexLiveBackoffIsDeterministic(t *testing.T) {
	got := deterministicBackoff(4, 100*time.Millisecond)
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deterministicBackoff = %v, want %v", got, want)
	}
}

func TestCodexLiveKeepsReplayMarkerWhenTurnStartAmbiguous(t *testing.T) {
	root := testRoot(t)
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{
			{err: context.DeadlineExceeded},
		},
	}
	a, err := New(WithLiveRoot(root), WithClient(fake), WithClock(&clock.Fake{T: time.Unix(100, 0).UTC()}), WithBackoff(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = a.Launch(context.Background(), container.Handle{}, testInvocation(t.TempDir(), "ambiguous"))
	var launchErr *agent.ErrAgentLaunch
	if !errors.As(err, &launchErr) {
		t.Fatalf("Launch err = %v, want *ErrAgentLaunch", err)
	}
	rec, err := live.ReadSessionRecord(root, AdapterRef, "ambiguous")
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if rec.ActiveTurn == nil {
		t.Fatalf("ActiveTurn cleared after ambiguous StartTurn error; want replay marker retained")
	}
}

func TestCodexLivePermissionDefaultDeny(t *testing.T) {
	root := testRoot(t)
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				{Type: EventPermissionRequest, Permission: &PermissionRequest{ID: "p1", Kind: "file", ToolID: "edit", Path: filepath.Join(t.TempDir(), "x.txt")}},
			},
		}},
	}
	a := newTestAdapter(t, root, fake)
	outcome := drainLaunch(t, a, testInvocation(t.TempDir(), "deny"))
	if !errors.Is(outcome.Err, agent.ErrPermissionDenied) {
		t.Fatalf("outcome err = %v, want ErrPermissionDenied", outcome.Err)
	}
	if len(fake.permissionResponses) != 1 || fake.permissionResponses[0].Allow {
		t.Fatalf("permission responses = %+v, want one deny", fake.permissionResponses)
	}
}

func TestLivePermissionPolicyAllowlistAllowsProviderRequest(t *testing.T) {
	root := testRoot(t)
	cwd := t.TempDir()
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				{Type: EventPermissionRequest, Permission: &PermissionRequest{ID: "p1", Kind: "file", ToolID: "edit", Path: filepath.Join(cwd, "x.txt")}},
				{Type: EventTurnCompleted, Output: map[string]any{"ok": true}},
			},
		}},
	}
	a := newTestAdapter(t, root, fake)
	inv := testInvocation(cwd, "allow")
	inv.With["permission_policy"] = map[string]any{
		"allow": []any{map[string]any{
			"kind":       "file",
			"tool_id":    "edit",
			"path_roots": []any{cwd},
		}},
	}
	outcome := drainLaunch(t, a, inv)
	if outcome.Err != nil {
		t.Fatalf("Launch outcome err: %v", outcome.Err)
	}
	if len(fake.permissionResponses) != 1 || !fake.permissionResponses[0].Allow {
		t.Fatalf("permission responses = %+v, want one allow", fake.permissionResponses)
	}
}

func TestLivePermissionPolicyRejectsSymlinkEscape(t *testing.T) {
	root := testRoot(t)
	allowedRoot := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(allowedRoot, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				{Type: EventPermissionRequest, Permission: &PermissionRequest{ID: "p1", Kind: "file", ToolID: "edit", Path: filepath.Join(link, "x.txt")}},
			},
		}},
	}
	a := newTestAdapter(t, root, fake)
	inv := testInvocation(allowedRoot, "symlink-deny")
	inv.With["permission_policy"] = map[string]any{
		"allow": []any{map[string]any{
			"kind":       "file",
			"tool_id":    "edit",
			"path_roots": []any{allowedRoot},
		}},
	}
	outcome := drainLaunch(t, a, inv)
	if !errors.Is(outcome.Err, agent.ErrPermissionDenied) {
		t.Fatalf("outcome err = %v, want ErrPermissionDenied", outcome.Err)
	}
	if len(fake.permissionResponses) != 1 || fake.permissionResponses[0].Allow {
		t.Fatalf("permission responses = %+v, want one deny", fake.permissionResponses)
	}
}

func TestLivePermissionPolicyRejectsCommandPrefixAsSecurityBoundary(t *testing.T) {
	a := newVersionTestAdapter(t, &fakeClient{info: ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"}})
	err := a.ValidateConfig(ir.RawConfig{
		"prompt":  "build",
		"cwd":     t.TempDir(),
		"session": "cmd",
		"permission_policy": map[string]any{
			"allow": []any{map[string]any{
				"kind":           "command",
				"command_prefix": "go test",
			}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "command_prefix") {
		t.Fatalf("ValidateConfig err = %v, want command_prefix rejection", err)
	}
}

func TestCodexLiveValidateConfigRejectsRelativeOrControlCWD(t *testing.T) {
	a := newVersionTestAdapter(t, &fakeClient{info: ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"}})
	for name, cwd := range map[string]string{
		"relative": "relative/path",
		"nul":      string([]byte{'/', 't', 'm', 'p', 0, 'x'}),
		"newline":  "/tmp/awf\nx",
	} {
		t.Run(name, func(t *testing.T) {
			err := a.ValidateConfig(ir.RawConfig{
				"prompt":  "build",
				"cwd":     cwd,
				"session": "cwd-check-" + name,
			})
			if err == nil {
				t.Fatal("ValidateConfig succeeded, want cwd rejection")
			}
			var cfgErr *agent.ErrInvalidConfig
			if !errors.As(err, &cfgErr) || cfgErr.Key != "cwd" {
				t.Fatalf("ValidateConfig err = %v, want cwd ErrInvalidConfig", err)
			}
		})
	}
}

func TestCodexLiveFailedTurnDoesNotCommitSuccess(t *testing.T) {
	root := testRoot(t)
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				{Type: EventTurnCompleted, Status: "failed", Error: "rate limit"},
			},
		}},
	}
	a := newTestAdapter(t, root, fake)
	outcome := drainLaunch(t, a, testInvocation(t.TempDir(), "failed-turn"))
	var launchErr *agent.ErrAgentLaunch
	if !errors.As(outcome.Err, &launchErr) {
		t.Fatalf("outcome err = %v, want *ErrAgentLaunch", outcome.Err)
	}
	if outcome.Result.ExitCode != 0 || outcome.Result.Live != nil {
		t.Fatalf("outcome result = %+v, want no success result", outcome.Result)
	}
}

func TestCodexLiveDoesNotReturnRawTranscript(t *testing.T) {
	root := testRoot(t)
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				{Type: EventItemCompleted, Text: `{"ok":true,"secret":"sk-secret"}`},
				{Type: EventTurnCompleted, Output: map[string]any{"ok": true}},
			},
		}},
	}
	a := newTestAdapter(t, root, fake)
	outcome := drainLaunch(t, a, testInvocation(t.TempDir(), "no-transcript"))
	if outcome.Err != nil {
		t.Fatalf("Launch outcome err: %v", outcome.Err)
	}
	if outcome.Result.Transcript != (agent.ThreadTurn{}) {
		t.Fatalf("Transcript = %+v, want zero value for live adapter", outcome.Result.Transcript)
	}
}

func TestCodexLiveEmitsOnlyRedactedEvents(t *testing.T) {
	root := testRoot(t)
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{
				{Type: EventAgentMessageDelta, Text: `token="sk-secret" Authorization: Bearer abc123`},
				{Type: EventTurnCompleted, Output: map[string]any{"ok": true}},
			},
		}},
	}
	a := newTestAdapter(t, root, fake)
	events, outcome := drainLaunchWithEvents(t, a, testInvocation(t.TempDir(), "redact"))
	if outcome.Err != nil {
		t.Fatalf("Launch outcome err: %v", outcome.Err)
	}
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	for _, ev := range events {
		if !ev.Live {
			t.Fatalf("event Live = false: %+v", ev)
		}
		payload := string(ev.Payload)
		if strings.Contains(payload, "sk-secret") || strings.Contains(payload, "abc123") {
			t.Fatalf("unredacted payload: %s", payload)
		}
		if !json.Valid(ev.Payload) {
			t.Fatalf("payload is not normalized JSON: %s", payload)
		}
	}
}

func TestCodexLiveReplayRequiredBeforeSecondPrompt(t *testing.T) {
	root := testRoot(t)
	cwd := t.TempDir()
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
	}
	rec := live.SessionRecord{
		AdapterRef:             AdapterRef,
		SessionKey:             "replay",
		CWD:                    cwd,
		CanonicalCWD:           mustEvalSymlinks(t, cwd),
		ProviderSessionID:      "thread-1",
		ProviderBinary:         "/bin/codex",
		ProviderProtocolSchema: AppServerSchemaDigest(),
		ActiveTurn: &live.ActiveTurn{
			Phase:        live.PhaseIntentRecorded,
			RunID:        "run-1",
			NodePath:     "node-1",
			CurrentEpoch: 1,
			NextEpoch:    1,
			PromptDigest: "sha256:prompt",
			LeaseID:      "lease-1",
		},
	}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	a := newTestAdapter(t, root, fake)
	inv := testInvocation(cwd, "replay")
	_, _, err := a.Launch(context.Background(), container.Handle{}, inv)
	if !errors.Is(err, agent.ErrLiveReplayRequired) {
		t.Fatalf("Launch err = %v, want ErrLiveReplayRequired", err)
	}
	if len(fake.turnRequests) != 0 {
		t.Fatalf("turn requests = %d, want 0", len(fake.turnRequests))
	}
	err = a.PreflightResume(context.Background(), agent.LiveResumePreflightRequest{
		NodePath:     inv.NodePath,
		AdapterRef:   AdapterRef,
		With:         inv.With,
		RunID:        inv.RunContext.RunID,
		CurrentEpoch: inv.RunContext.CurrentEpoch,
		NextEpoch:    inv.RunContext.NextEpoch,
	})
	if !errors.Is(err, agent.ErrLiveReplayRequired) {
		t.Fatalf("PreflightResume err = %v, want ErrLiveReplayRequired", err)
	}
}

func TestCodexLiveSessionDriftFailsBeforeProviderResume(t *testing.T) {
	root := testRoot(t)
	originalCWD := t.TempDir()
	nextCWD := t.TempDir()
	fake := &fakeClient{
		info: ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
	}
	rec := live.SessionRecord{
		AdapterRef:             AdapterRef,
		SessionKey:             "drift",
		CWD:                    originalCWD,
		CanonicalCWD:           mustEvalSymlinks(t, originalCWD),
		ProviderSessionID:      "thread-1",
		ProviderBinary:         "/bin/codex",
		ProviderProtocolSchema: AppServerSchemaDigest(),
	}
	if err := live.WriteSessionRecord(root, rec); err != nil {
		t.Fatalf("WriteSessionRecord: %v", err)
	}
	a := newTestAdapter(t, root, fake)
	_, _, err := a.Launch(context.Background(), container.Handle{}, testInvocation(nextCWD, "drift"))
	if !errors.Is(err, live.ErrSessionDrift) {
		t.Fatalf("Launch err = %v, want ErrSessionDrift", err)
	}
	if fake.resumes != 0 {
		t.Fatalf("resumes = %d, want 0 before drift rejection", fake.resumes)
	}
	if len(fake.turnRequests) != 0 {
		t.Fatalf("turn requests = %d, want 0 before drift rejection", len(fake.turnRequests))
	}
}

func TestCodexLiveResultCarriesFinalizerHandoff(t *testing.T) {
	root := testRoot(t)
	fake := &fakeClient{
		info:        ProviderInfo{Version: "codex-cli/0.137.0", Binary: "/bin/codex"},
		startThread: ThreadInfo{ID: "thread-1"},
		turns: []fakeTurn{{
			turnID: "turn-1",
			events: []ProviderEvent{{Type: EventTurnCompleted, Output: map[string]any{"ok": true}}},
		}},
	}
	a := newTestAdapter(t, root, fake)
	inv := testInvocation(t.TempDir(), "finalize")
	outcome := drainLaunch(t, a, inv)
	meta := outcome.Result.Live
	if meta == nil {
		t.Fatal("AgentResult.Live = nil")
	}
	if meta.AdapterRef != AdapterRef || meta.SessionKey != "finalize" || meta.ProviderTurnID != "turn-1" || meta.Epoch != inv.RunContext.NextEpoch {
		t.Fatalf("metadata = %+v", *meta)
	}
	if meta.SessionKeyHash == "" || meta.LeaseID == "" || meta.ActiveTurnID == "" || meta.CommittedUnix == 0 {
		t.Fatalf("metadata missing durable IDs: %+v", *meta)
	}
	if err := finalizeLiveForTest(root, meta, 1234); err != nil {
		t.Fatalf("FinalizeCommittedTurn: %v", err)
	}
	rec, err := live.ReadSessionRecord(root, AdapterRef, "finalize")
	if err != nil {
		t.Fatalf("ReadSessionRecord: %v", err)
	}
	if rec.ActiveTurn != nil || rec.LastCommittedTurn == nil || rec.LastCommittedTurn.ProviderTurnID != "turn-1" {
		t.Fatalf("record after finalizer = %+v", rec)
	}
}

func finalizeLiveForTest(root live.Root, meta *agent.LiveDispatch, committedUnix int64) error {
	if err := live.FinalizeCommittedTurn(root, meta.AdapterRef, meta.SessionKey, live.CommittedTurn{
		RunID:          meta.RunID,
		NodePath:       meta.NodePath,
		Epoch:          int(meta.Epoch),
		ProviderTurnID: meta.ProviderTurnID,
		CommittedUnix:  committedUnix,
	}); err != nil {
		return err
	}
	return live.ReleaseLease(root, meta.AdapterRef, meta.SessionKey, meta.LeaseID)
}

func testRoot(t *testing.T) live.Root {
	t.Helper()
	root, err := live.OpenRoot(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	return root
}

func newTestAdapter(t *testing.T, root live.Root, c *fakeClient) *Adapter {
	t.Helper()
	a, err := New(WithLiveRoot(root), WithClient(c), WithClock(&clock.Fake{T: time.Unix(100, 0).UTC()}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func newVersionTestAdapter(t *testing.T, c *fakeClient) *Adapter {
	t.Helper()
	a, err := New(WithClient(c))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func testInvocation(cwd, session string) agent.AgentInvocation {
	return agent.AgentInvocation{
		NodePath: "node-1",
		Uses:     AdapterRef,
		RunContext: agent.RunContext{
			RunID:        "run-1",
			CurrentEpoch: 1,
			NextEpoch:    1,
		},
		With: ir.RawConfig{
			"prompt":  "please build",
			"cwd":     cwd,
			"session": session,
		},
	}
}

func drainLaunch(t *testing.T, a *Adapter, inv agent.AgentInvocation) agent.AgentOutcome {
	t.Helper()
	_, outcome := drainLaunchWithEvents(t, a, inv)
	return outcome
}

func drainLaunchWithEvents(t *testing.T, a *Adapter, inv agent.AgentInvocation) ([]agent.AgentEvent, agent.AgentOutcome) {
	t.Helper()
	events, outcomeCh, err := a.Launch(context.Background(), container.Handle{}, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	outcome := <-outcomeCh
	return got, outcome
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return got
}

type fakeTurn struct {
	turnID string
	events []ProviderEvent
	err    error
}

type fakeClient struct {
	info                ProviderInfo
	startThread         ThreadInfo
	turns               []fakeTurn
	starts              int
	resumes             int
	resumeThreadIDs     []string
	turnRequests        []TurnStartRequest
	permissionResponses []PermissionResponse
}

func (f *fakeClient) ProviderInfo(context.Context) (ProviderInfo, error) {
	return f.info, nil
}

func (f *fakeClient) StartThread(_ context.Context, req ThreadStartRequest) (ThreadInfo, error) {
	f.starts++
	if f.startThread.ID == "" {
		f.startThread.ID = "thread-started"
	}
	return f.startThread, nil
}

func (f *fakeClient) ResumeThread(_ context.Context, req ThreadResumeRequest) (ThreadInfo, error) {
	f.resumes++
	f.resumeThreadIDs = append(f.resumeThreadIDs, req.ThreadID)
	return ThreadInfo{ID: req.ThreadID}, nil
}

func (f *fakeClient) StartTurn(_ context.Context, req TurnStartRequest) (TurnHandle, error) {
	f.turnRequests = append(f.turnRequests, req)
	if len(f.turns) == 0 {
		return TurnHandle{}, errors.New("unexpected StartTurn")
	}
	turn := f.turns[0]
	f.turns = f.turns[1:]
	if turn.err != nil {
		return TurnHandle{}, turn.err
	}
	ch := make(chan ProviderEvent, len(turn.events))
	for _, ev := range turn.events {
		ch <- ev
	}
	close(ch)
	return TurnHandle{TurnID: turn.turnID, Events: ch}, nil
}

func (f *fakeClient) RespondPermission(_ context.Context, resp PermissionResponse) error {
	f.permissionResponses = append(f.permissionResponses, resp)
	return nil
}

type recordingClock struct {
	clock.Clock
	sleeps []time.Duration
}

func (c *recordingClock) Sleep(ctx context.Context, d time.Duration) error {
	c.sleeps = append(c.sleeps, d)
	return c.Clock.Sleep(ctx, d)
}
