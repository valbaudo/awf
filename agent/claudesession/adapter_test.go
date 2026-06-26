package claudesession_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claudesession"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// ---- construction + ref ----

func TestNew_DefaultConstruct(t *testing.T) {
	a, err := claudesession.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a == nil {
		t.Fatal("New returned nil adapter")
	}
}

func TestAdapter_Ref(t *testing.T) {
	a, _ := claudesession.New()
	if a.Ref() != claudesession.AdapterRef {
		t.Errorf("Ref() = %q, want %q", a.Ref(), claudesession.AdapterRef)
	}
	if a.Ref() != "anthropic/claude-code-session" {
		t.Errorf("Ref() = %q; expected literal 'anthropic/claude-code-session'", a.Ref())
	}
}

// ---- capabilities ----

func TestAdapter_Capabilities_NativeSchemaAndPersistentSession(t *testing.T) {
	a, _ := claudesession.New()
	caps := a.Capabilities()
	if !caps.NativeSchema {
		t.Error("Capabilities().NativeSchema = false; want true")
	}
	if !caps.PersistentSession {
		t.Error("Capabilities().PersistentSession = false; want true")
	}
	if caps.Containerless {
		t.Error("Capabilities().Containerless = true; want false (container-backed)")
	}
}

// ---- registry resolution ----

func TestAdapter_ResolvesViaAgentRegistry(t *testing.T) {
	var reg agent.Registry
	a, _ := claudesession.New()
	if err := reg.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := reg.Lookup(claudesession.AdapterRef)
	if !ok {
		t.Fatalf("Lookup(%q) = false", claudesession.AdapterRef)
	}
	if got.Ref() != claudesession.AdapterRef {
		t.Errorf("Ref() = %q", got.Ref())
	}
	if !got.Capabilities().PersistentSession {
		t.Error("looked-up adapter: PersistentSession = false")
	}
}

// ---- ValidateConfig ----

func TestValidateConfig_HappyPath_MinimalPrompt(t *testing.T) {
	a, _ := claudesession.New(claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "do the thing"}); err != nil {
		t.Errorf("ValidateConfig: %v", err)
	}
}

func TestValidateConfig_HappyPath_AllOptionalKeys(t *testing.T) {
	a, _ := claudesession.New(claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}))
	err := a.ValidateConfig(ir.RawConfig{
		"prompt":         "p",
		"model":          "claude-opus-4-7",
		"effort":         "max",
		"max_turns":      10,
		"system_prompt":  "you are X",
		"allowed_tools":  []any{"Bash", "Read"},
		"bare":           true,
		"max_budget_usd": 5.0,
		"workdir":        "/work/proj",
	})
	if err != nil {
		t.Errorf("ValidateConfig: %v", err)
	}
}

func TestValidateConfig_MissingPrompt_Rejects(t *testing.T) {
	a, _ := claudesession.New(claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	err := a.ValidateConfig(ir.RawConfig{"model": "claude-opus"})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v; want *agent.ErrInvalidConfig", err)
	}
	if bad.Key != "prompt" {
		t.Errorf("Key = %q; want %q", bad.Key, "prompt")
	}
}

func TestValidateConfig_UnknownKey_Rejects(t *testing.T) {
	a, _ := claudesession.New(claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "verbose": true})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v; want *agent.ErrInvalidConfig", err)
	}
	if bad.Key != "verbose" {
		t.Errorf("Key = %q; want %q", bad.Key, "verbose")
	}
}

func TestValidateConfig_BareTrue_NoAPIKey_Rejects(t *testing.T) {
	a, _ := claudesession.New(claudesession.WithEnv(map[string]string{"OTHER": "v"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "bare": true})
	var bare *claudesession.ErrBareRequiresAPIKey
	if !errors.As(err, &bare) {
		t.Fatalf("err = %v; want *claudesession.ErrBareRequiresAPIKey", err)
	}
}

func TestValidateConfig_BareTrue_WithAuthToken_OK(t *testing.T) {
	a, _ := claudesession.New(claudesession.WithEnv(map[string]string{"ANTHROPIC_AUTH_TOKEN": "x"}))
	if err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "bare": true}); err != nil {
		t.Errorf("ValidateConfig: %v (ANTHROPIC_AUTH_TOKEN should satisfy bare requirement)", err)
	}
}

// TestValidateConfig_SessionReuseKeysRejectedAsUnknown verifies that session_id
// is rejected as an unknown with-key. The session adapter derives the UUID
// deterministically from the invocation; the workflow author cannot override it.
// Unlike the base claude adapter (which rejects session_id as a session-reuse
// attempt), this adapter rejects it because it is simply not a known key.
func TestValidateConfig_SessionReuseKeysRejectedAsUnknown(t *testing.T) {
	// The session adapter does NOT accept session_id as a user-visible with-key.
	// The UUID is derived internally from the invocation; the workflow author
	// cannot override it. session_id is therefore an unknown-key rejection here.
	a, _ := claudesession.New(claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	err := a.ValidateConfig(ir.RawConfig{"prompt": "p", "session_id": "abc"})
	var bad *agent.ErrInvalidConfig
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v; want *agent.ErrInvalidConfig (session_id is an unknown with-key)", err)
	}
}

// ---- ResumePreflighter ----

// TestAdapter_ImplementsResumePreflighter asserts that *Adapter satisfies
// agent.ResumePreflighter (no-op stub, real logic deferred to M2d).
func TestAdapter_ImplementsResumePreflighter(t *testing.T) {
	a, _ := claudesession.New()
	var _ agent.ResumePreflighter = a // compile-time; belt-and-suspenders at runtime:
	if _, ok := any(a).(agent.ResumePreflighter); !ok {
		t.Fatal("*Adapter does not implement agent.ResumePreflighter")
	}
}

// TestAdapter_PreflightResume_NoOp asserts that PreflightResume always returns nil.
func TestAdapter_PreflightResume_NoOp(t *testing.T) {
	a, _ := claudesession.New()
	req := agent.LiveResumePreflightRequest{}
	if err := a.PreflightResume(context.Background(), req); err != nil {
		t.Fatalf("PreflightResume returned non-nil error: %v", err)
	}
}

// ---- sessionUUID (via export_test.go) ----

func TestSessionUUID_Deterministic(t *testing.T) {
	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		RunContext: agent.RunContext{
			RunID:        "run-abc",
			CurrentEpoch: 1,
		},
	}
	u1 := claudesession.SessionUUIDForTest(inv)
	u2 := claudesession.SessionUUIDForTest(inv)
	if u1 != u2 {
		t.Errorf("sessionUUID non-deterministic: %q != %q", u1, u2)
	}
}

func TestSessionUUID_DifferentNodePath_DifferentUUID(t *testing.T) {
	base := agent.RunContext{RunID: "run-abc", CurrentEpoch: 1}
	inv1 := agent.AgentInvocation{NodePath: "graph[0]", RunContext: base}
	inv2 := agent.AgentInvocation{NodePath: "graph[1]", RunContext: base}
	u1 := claudesession.SessionUUIDForTest(inv1)
	u2 := claudesession.SessionUUIDForTest(inv2)
	if u1 == u2 {
		t.Errorf("different NodePath produced same UUID %q", u1)
	}
}

func TestSessionUUID_DifferentRunID_DifferentUUID(t *testing.T) {
	inv1 := agent.AgentInvocation{NodePath: "graph[0]", RunContext: agent.RunContext{RunID: "run-1", CurrentEpoch: 0}}
	inv2 := agent.AgentInvocation{NodePath: "graph[0]", RunContext: agent.RunContext{RunID: "run-2", CurrentEpoch: 0}}
	u1 := claudesession.SessionUUIDForTest(inv1)
	u2 := claudesession.SessionUUIDForTest(inv2)
	if u1 == u2 {
		t.Errorf("different RunID produced same UUID %q", u1)
	}
}

func TestSessionUUID_Format_IsUUIDShaped(t *testing.T) {
	inv := agent.AgentInvocation{NodePath: "graph[0]", RunContext: agent.RunContext{RunID: "r", CurrentEpoch: 0}}
	u := claudesession.SessionUUIDForTest(inv)
	// Expect 8-4-4-4-12 hex groups separated by dashes (36 chars).
	parts := strings.Split(u, "-")
	if len(parts) != 5 {
		t.Fatalf("UUID %q: expected 5 dash-separated groups, got %d", u, len(parts))
	}
	for i, want := range []int{8, 4, 4, 4, 12} {
		if len(parts[i]) != want {
			t.Errorf("UUID group %d: len=%d, want %d (full uuid: %q)", i, len(parts[i]), want, u)
		}
	}
}

// ---- Launch: --session-id in command ----

func TestLaunch_CommandContainsSessionID(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"ok":true}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})

	a, _ := claudesession.New(
		claudesession.WithBackend(f),
		claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}),
	)
	inv := agent.AgentInvocation{
		NodePath:   "graph[0]",
		Uses:       claudesession.AdapterRef,
		RunContext: agent.RunContext{RunID: "run-xyz", CurrentEpoch: 2},
		With:       ir.RawConfig{"prompt": "do stuff"},
		OutputSchema: &ir.JSONSchema{
			"type": "object", "additionalProperties": false,
			"required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
		},
	}

	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range eventCh {
	}
	if outcome := <-outcomeCh; outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}

	if len(f.Calls) == 0 {
		t.Fatal("no recorded calls")
	}
	cmd := f.Calls[0].Run

	expectedUUID := claudesession.SessionUUIDForTest(inv)
	if !strings.Contains(cmd, "--session-id") {
		t.Errorf("command missing --session-id: %s", cmd)
	}
	if !strings.Contains(cmd, expectedUUID) {
		t.Errorf("command missing expected uuid %q: %s", expectedUUID, cmd)
	}
	// --resume MUST be absent on a fresh turn (ResumeSession=false).
	if strings.Contains(cmd, "--resume") {
		t.Errorf("command contains --resume on fresh turn (ResumeSession=false); must use --session-id only: %s", cmd)
	}
	// --no-session-persistence MUST be absent (session adapter persists sessions).
	if strings.Contains(cmd, "--no-session-persistence") {
		t.Errorf("command contains --no-session-persistence (must be absent for session adapter): %s", cmd)
	}
	if !strings.Contains(cmd, "--output-format stream-json") {
		t.Errorf("command missing --output-format stream-json: %s", cmd)
	}
}

// TestLaunch_ResumeSession_CommandContainsResume verifies that when
// inv.ResumeSession is true (the engine has restored a session transcript),
// the adapter passes --resume <uuid> and NOT --session-id.
func TestLaunch_ResumeSession_CommandContainsResume(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"ok":true}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})

	a, _ := claudesession.New(
		claudesession.WithBackend(f),
		claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}),
	)
	inv := agent.AgentInvocation{
		NodePath:   "graph[0]",
		Uses:       claudesession.AdapterRef,
		RunContext: agent.RunContext{RunID: "run-xyz", CurrentEpoch: 2},
		With:       ir.RawConfig{"prompt": "do stuff"},
		OutputSchema: &ir.JSONSchema{
			"type": "object", "additionalProperties": false,
			"required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
		},
		ResumeSession: true, // engine restored a transcript for this node
	}

	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range eventCh {
	}
	if outcome := <-outcomeCh; outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}

	if len(f.Calls) == 0 {
		t.Fatal("no recorded calls")
	}
	cmd := f.Calls[0].Run

	expectedUUID := claudesession.SessionUUIDForTest(inv)
	// --resume <uuid> must be present on a restored turn.
	if !strings.Contains(cmd, "--resume") {
		t.Errorf("command missing --resume on restored turn (ResumeSession=true): %s", cmd)
	}
	if !strings.Contains(cmd, expectedUUID) {
		t.Errorf("command missing expected uuid %q: %s", expectedUUID, cmd)
	}
	// --session-id MUST be absent (only one of --resume or --session-id is passed).
	if strings.Contains(cmd, "--session-id") {
		t.Errorf("command contains --session-id on restored turn (ResumeSession=true); must use --resume only: %s", cmd)
	}
	// --no-session-persistence MUST be absent.
	if strings.Contains(cmd, "--no-session-persistence") {
		t.Errorf("command contains --no-session-persistence (must be absent for session adapter): %s", cmd)
	}
}

// TestLaunch_SessionConfigDir_EnvInjection verifies that when
// inv.SessionConfigDir is non-empty, the adapter sets both
// CLAUDE_CONFIG_DIR and CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC in the
// Cmd.Env forwarded to the backend.
func TestLaunch_SessionConfigDir_EnvInjection(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"ok":true}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})

	a, _ := claudesession.New(
		claudesession.WithBackend(f),
		claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}),
	)
	inv := agent.AgentInvocation{
		NodePath:         "graph[0]",
		Uses:             claudesession.AdapterRef,
		RunContext:       agent.RunContext{RunID: "run-xyz", CurrentEpoch: 1},
		With:             ir.RawConfig{"prompt": "do stuff"},
		SessionConfigDir: "/staging/claude-session/run-xyz",
	}

	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range eventCh {
	}
	if outcome := <-outcomeCh; outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}

	if len(f.Calls) == 0 {
		t.Fatal("no recorded calls")
	}
	env := f.Calls[0].Env

	if got := env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"]; got != "1" {
		t.Errorf("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = %q; want %q", got, "1")
	}
	if got := env["CLAUDE_CONFIG_DIR"]; got != "/staging/claude-session/run-xyz" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q; want %q", got, "/staging/claude-session/run-xyz")
	}
}

// TestLaunch_EmptySessionConfigDir_NoClaudeConfigDir verifies that when
// inv.SessionConfigDir is empty, CLAUDE_CONFIG_DIR is NOT set in the Cmd.Env,
// but CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC is always set.
func TestLaunch_EmptySessionConfigDir_NoClaudeConfigDir(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"ok":true}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})

	a, _ := claudesession.New(
		claudesession.WithBackend(f),
		claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}),
	)
	inv := agent.AgentInvocation{
		NodePath:   "graph[0]",
		Uses:       claudesession.AdapterRef,
		RunContext: agent.RunContext{RunID: "run-xyz", CurrentEpoch: 1},
		With:       ir.RawConfig{"prompt": "do stuff"},
		// SessionConfigDir intentionally empty
	}

	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for range eventCh {
	}
	if outcome := <-outcomeCh; outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}

	if len(f.Calls) == 0 {
		t.Fatal("no recorded calls")
	}
	env := f.Calls[0].Env

	if got := env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"]; got != "1" {
		t.Errorf("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = %q; want %q", got, "1")
	}
	if _, ok := env["CLAUDE_CONFIG_DIR"]; ok {
		t.Errorf("CLAUDE_CONFIG_DIR unexpectedly set to %q; should be absent when SessionConfigDir is empty", env["CLAUDE_CONFIG_DIR"])
	}
}

// TestLaunch_EnvNotMutatedAcrossTwoLaunches verifies that the adapter's
// stored a.env is not mutated by Launch. If the env map were mutated, keys
// injected during the first Launch (e.g. CLAUDE_CONFIG_DIR) would bleed into
// the second Launch's Cmd.Env even when the second invocation has an empty
// SessionConfigDir.
func TestLaunch_EnvNotMutatedAcrossTwoLaunches(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"ok":true}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})

	a, _ := claudesession.New(
		claudesession.WithBackend(f),
		claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}),
	)

	// First Launch: SessionConfigDir is set.
	inv1 := agent.AgentInvocation{
		NodePath:         "graph[0]",
		Uses:             claudesession.AdapterRef,
		RunContext:       agent.RunContext{RunID: "run-xyz", CurrentEpoch: 1},
		With:             ir.RawConfig{"prompt": "first"},
		SessionConfigDir: "/staging/claude-session/run-xyz",
	}
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv1)
	if err != nil {
		t.Fatalf("Launch #1: %v", err)
	}
	for range eventCh {
	}
	if outcome := <-outcomeCh; outcome.Err != nil {
		t.Fatalf("Launch #1 outcome.Err = %v", outcome.Err)
	}

	// Second Launch: SessionConfigDir is empty — CLAUDE_CONFIG_DIR must not bleed.
	inv2 := agent.AgentInvocation{
		NodePath:   "graph[1]",
		Uses:       claudesession.AdapterRef,
		RunContext: agent.RunContext{RunID: "run-xyz", CurrentEpoch: 1},
		With:       ir.RawConfig{"prompt": "second"},
		// SessionConfigDir intentionally empty
	}
	eventCh2, outcomeCh2, err2 := a.Launch(context.Background(), h, inv2)
	if err2 != nil {
		t.Fatalf("Launch #2: %v", err2)
	}
	for range eventCh2 {
	}
	if outcome := <-outcomeCh2; outcome.Err != nil {
		t.Fatalf("Launch #2 outcome.Err = %v", outcome.Err)
	}

	if len(f.Calls) < 2 {
		t.Fatalf("expected 2 recorded calls, got %d", len(f.Calls))
	}

	// First call must have CLAUDE_CONFIG_DIR.
	if got := f.Calls[0].Env["CLAUDE_CONFIG_DIR"]; got != "/staging/claude-session/run-xyz" {
		t.Errorf("Launch #1 CLAUDE_CONFIG_DIR = %q; want %q", got, "/staging/claude-session/run-xyz")
	}

	// Second call must NOT have CLAUDE_CONFIG_DIR (proving a.env was not mutated).
	if _, ok := f.Calls[1].Env["CLAUDE_CONFIG_DIR"]; ok {
		t.Errorf("Launch #2 CLAUDE_CONFIG_DIR = %q unexpectedly present; a.env was mutated by Launch #1", f.Calls[1].Env["CLAUDE_CONFIG_DIR"])
	}

	// Both calls must have CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC.
	for i, c := range f.Calls[:2] {
		if got := c.Env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"]; got != "1" {
			t.Errorf("Launch #%d CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = %q; want %q", i+1, got, "1")
		}
	}
}

func TestLaunch_NoBackend_Errors(t *testing.T) {
	a, _ := claudesession.New() // no WithBackend
	h := container.Handle{Name: "lab"}
	inv := agent.AgentInvocation{
		NodePath: "graph[0]",
		Uses:     claudesession.AdapterRef,
		With:     ir.RawConfig{"prompt": "x"},
	}
	_, _, err := a.Launch(context.Background(), h, inv)
	if err == nil {
		t.Fatal("expected error when no backend wired")
	}
	var launchErr *agent.ErrAgentLaunch
	if !errors.As(err, &launchErr) {
		t.Errorf("err = %T %v; want *agent.ErrAgentLaunch", err, err)
	}
}

// TestLaunch_ConcurrentEnvMutation verifies that concurrent calls to Launch
// from multiple goroutines do not mutate the adapter's shared a.env map.
// Covers two failure modes:
//  1. Data race: concurrent writes to the same map detected by -race.
//  2. Key leakage: AWF_IDEMPOTENCY_KEY / CLAUDE_CONFIG_DIR /
//     CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC must not appear in a.env after
//     any number of launches.
//
// Run with: go test -race ./agent/claudesession/...
func TestLaunch_ConcurrentEnvMutation(t *testing.T) {
	const goroutines = 8

	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	streamLines := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"ok":true}}` + "\n")
	// ProgramExecAny matches any Cmd.Run — one call serves all concurrent launches.
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: streamLines}, []container.IOChunk{
		{Stream: "stdout", Data: streamLines},
	})

	a, _ := claudesession.New(
		claudesession.WithBackend(f),
		claudesession.WithEnv(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}),
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			inv := agent.AgentInvocation{
				NodePath:         fmt.Sprintf("graph[%d]", i),
				Uses:             claudesession.AdapterRef,
				RunContext:       agent.RunContext{RunID: "run-race", CurrentEpoch: uint32(i)},
				With:             ir.RawConfig{"prompt": "concurrent"},
				IdempotencyKey:   fmt.Sprintf("idem-%d", i),
				SessionConfigDir: fmt.Sprintf("/staging/cfg/%d", i),
			}
			eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
			if err != nil {
				t.Errorf("goroutine %d: Launch: %v", i, err)
				return
			}
			for range eventCh {
			}
			if outcome := <-outcomeCh; outcome.Err != nil {
				t.Errorf("goroutine %d: outcome.Err = %v", i, outcome.Err)
			}
		}()
	}
	wg.Wait()

	// After all launches, a.env must not contain any per-invocation keys.
	env := claudesession.AdapterEnvForTest(a)
	forbidden := []string{
		"AWF_IDEMPOTENCY_KEY",
		"CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
	}
	for _, key := range forbidden {
		if _, ok := env[key]; ok {
			t.Errorf("a.env contains %q after concurrent launches; shared env was mutated", key)
		}
	}
}
