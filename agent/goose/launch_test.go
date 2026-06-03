package goose_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/goose"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

func gooseLaunchAdapter(t *testing.T, f *container.Fake) *goose.Adapter {
	t.Helper()
	a, err := goose.New(goose.WithBackend(f), goose.WithEnv(map[string]string{"GOOSE_PROVIDER": "claude-code"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func drainLaunch(t *testing.T, a *goose.Adapter, h container.Handle, inv agent.AgentInvocation) ([]agent.AgentEvent, agent.AgentOutcome) {
	t.Helper()
	eventCh, outcomeCh, err := a.Launch(context.Background(), h, inv)
	if err != nil {
		t.Fatalf("Launch (pre-launch err): %v", err)
	}
	var events []agent.AgentEvent
	for ev := range eventCh {
		events = append(events, ev)
	}
	return events, <-outcomeCh
}

func chunk(b []byte) container.IOChunk { return container.IOChunk{Stream: "stdout", Data: b} }
func nl(b []byte) []byte               { return append(append([]byte(nil), b...), '\n') }

func TestLaunch_HappyPath_TypedOutput_ReassembledDeltas(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{
		chunk(nl(msgLine("assistant", `{\"`))),
		chunk(nl(msgLine("assistant", `answer\": 4}`))),
		chunk(nl(completeLine(0, 0))),
	})
	a := gooseLaunchAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: goose.AdapterRef,
		With:         ir.RawConfig{"prompt": "what is 2+2"},
		OutputSchema: &ir.JSONSchema{"type": "object", "required": []string{"answer"}, "properties": map[string]any{"answer": map[string]any{"type": "integer"}}},
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if v, ok := outcome.Result.Output["answer"].(float64); !ok || v != 4 {
		t.Errorf("Output[answer] = %v, want 4", outcome.Result.Output["answer"])
	}
}

func TestLaunch_CommandLine_And_OpsecEnv(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(nl(msgLine("assistant", "ok"))), chunk(nl(completeLine(0, 0)))})
	a := gooseLaunchAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: goose.AdapterRef,
		With:           ir.RawConfig{"prompt": "do it", "model": "sonnet", "max_turns": float64(50), "system_prompt": "be terse"},
		IdempotencyKey: "idem-123",
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	cmd := f.Calls[0].Run
	for _, want := range []string{"goose run", "-q", "--output-format stream-json", "--no-session", "--model 'sonnet'", "--max-turns 50", "--system 'be terse'", "-t '"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q\nfull: %s", want, cmd)
		}
	}
	env := f.Calls[0].Env
	if env["GOOSE_MODE"] != "auto" || env["GOOSE_DISABLE_KEYRING"] != "1" || env["GOOSE_TELEMETRY_ENABLED"] != "false" {
		t.Errorf("opsec env not injected: %v", env)
	}
	if env["XDG_DATA_HOME"] == "" || env["XDG_STATE_HOME"] == "" {
		t.Errorf("XDG ephemeral-state env not injected: %v", env)
	}
	if env["AWF_IDEMPOTENCY_KEY"] != "idem-123" {
		t.Errorf("idempotency key not injected: %v", env)
	}
}

func TestLaunch_OmitsSessionFlags(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(nl(msgLine("assistant", "ok"))), chunk(nl(completeLine(0, 0)))})
	a := gooseLaunchAdapter(t, f)
	_, _ = drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: goose.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	for _, forbidden := range []string{"--resume", "--session-id", "--name", "--path", "--fork", "--interactive", " -r ", " -s "} {
		if strings.Contains(f.Calls[0].Run, forbidden) {
			t.Errorf("command contained forbidden flag %q: %s", forbidden, f.Calls[0].Run)
		}
	}
}

func TestLaunch_ConfigErrorStdout_Permanent(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{chunk([]byte("  error: Error Unknown provider: nope.\n"))})
	a := gooseLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: goose.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var bad *agent.ErrInvalidConfig
	if !errors.As(outcome.Err, &bad) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrInvalidConfig (permanent)", outcome.Err)
	}
}

func TestLaunch_BadModelZeroOutput_RetryableUnexpectedExit(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(nl(completeLine(0, 0)))})
	a := gooseLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: goose.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var unexp *goose.ErrUnexpectedExit
	if !errors.As(outcome.Err, &unexp) {
		t.Fatalf("outcome.Err = %v, want *goose.ErrUnexpectedExit (no-output trap)", outcome.Err)
	}
}

func TestLaunch_ErrorEvent_Retryable(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{chunk([]byte(`{"type":"error","error":"provider stream died"}` + "\n"))})
	a := gooseLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: goose.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var launch *agent.ErrAgentLaunch
	if !errors.As(outcome.Err, &launch) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrAgentLaunch (retryable)", outcome.Err)
	}
}

func TestLaunch_TransportError_AgentLaunch(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{Err: errors.New("backend exec died")}, nil)
	a := gooseLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: goose.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var launch *agent.ErrAgentLaunch
	if !errors.As(outcome.Err, &launch) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrAgentLaunch", outcome.Err)
	}
}

func TestLaunch_FeedbackAndSchema_Compose(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(nl(msgLine("assistant", `{\"ok\": true}`))), chunk(nl(completeLine(0, 0)))})
	a := gooseLaunchAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "gate[0].attempt-2.generate[0]", Uses: goose.AdapterRef,
		With:         ir.RawConfig{"prompt": "fix it"},
		Feedback:     ir.RawConfig{"reason": "tests failed"},
		OutputSchema: &ir.JSONSchema{"type": "object", "required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	cmd := f.Calls[0].Run
	vIdx, sIdx := strings.Index(cmd, "previous verdict"), strings.Index(cmd, "JSON Schema")
	if vIdx < 0 || !strings.Contains(cmd, "tests failed") {
		t.Errorf("feedback not prepended: %s", cmd)
	}
	if sIdx < 0 {
		t.Errorf("schema directive not appended: %s", cmd)
	}
	if vIdx >= 0 && sIdx >= 0 && vIdx > sIdx {
		t.Errorf("feedback must come BEFORE schema: verdict@%d schema@%d", vIdx, sIdx)
	}
}

func TestLaunch_NilBackend(t *testing.T) {
	a, _ := goose.New()
	_, _, err := a.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{With: ir.RawConfig{"prompt": "x"}})
	var launch *agent.ErrAgentLaunch
	if !errors.As(err, &launch) {
		t.Fatalf("err = %v, want *agent.ErrAgentLaunch", err)
	}
}

func TestShellQuote_EscapesSingleQuotes(t *testing.T) {
	if got, want := goose.ShellQuoteForTest("it's"), `'it'\''s'`; got != want {
		t.Errorf("shellQuote = %q, want %q", got, want)
	}
}
