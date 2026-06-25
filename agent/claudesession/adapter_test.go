package claudesession_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

// ---- encodeProjectDir ----

func TestEncodeProjectDir(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/work/proj", "-work-proj"},
		{"/", "-"},
		{"/home/user/.work", "-home-user--work"},
		{"/work/my_project", "-work-my-project"},
		{"/no/dots/or/underscores", "-no-dots-or-underscores"},
	}
	for _, tc := range cases {
		got := claudesession.EncodeProjectDirForTest(tc.in)
		if got != tc.want {
			t.Errorf("encodeProjectDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---- SessionTranscriptPath ----

func TestSessionTranscriptPath_ExpectedLayout(t *testing.T) {
	a, _ := claudesession.New(
		claudesession.WithHomeDir("/root"),
	)
	inv := agent.AgentInvocation{
		NodePath:   "graph[0]",
		RunContext: agent.RunContext{RunID: "run-abc", CurrentEpoch: 1},
	}
	workdir := "/work/proj"
	got := a.SessionTranscriptPath(inv, workdir)

	expectedUUID := claudesession.SessionUUIDForTest(inv)
	expectedEnc := claudesession.EncodeProjectDirForTest(workdir)
	want := filepath.Join("/root", ".claude", "projects", expectedEnc, expectedUUID+".jsonl")

	if got != want {
		t.Errorf("SessionTranscriptPath = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, ".jsonl") {
		t.Errorf("path does not end in .jsonl: %q", got)
	}
}

func TestSessionTranscriptPath_ContainsWorkdirEncoding(t *testing.T) {
	a, _ := claudesession.New(claudesession.WithHomeDir("/home/agent"))
	inv := agent.AgentInvocation{
		NodePath:   "graph[0]",
		RunContext: agent.RunContext{RunID: "r", CurrentEpoch: 0},
	}
	got := a.SessionTranscriptPath(inv, "/work/proj")
	if !strings.Contains(got, "-work-proj") {
		t.Errorf("path does not contain encoded workdir '-work-proj': %q", got)
	}
	if !strings.HasPrefix(got, "/home/agent/.claude/projects/") {
		t.Errorf("path does not start with home/.claude/projects/: %q", got)
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
	// --no-session-persistence MUST be absent (session adapter persists sessions).
	if strings.Contains(cmd, "--no-session-persistence") {
		t.Errorf("command contains --no-session-persistence (must be absent for session adapter): %s", cmd)
	}
	if !strings.Contains(cmd, "--output-format stream-json") {
		t.Errorf("command missing --output-format stream-json: %s", cmd)
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
