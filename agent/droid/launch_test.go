package droid_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/droid"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

func droidAdapter(t *testing.T, f *container.Fake) *droid.Adapter {
	t.Helper()
	a, err := droid.New(droid.WithBackend(f), droid.WithEnv(map[string]string{"FACTORY_API_KEY": "fk-test"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func drainLaunch(t *testing.T, a *droid.Adapter, h container.Handle, inv agent.AgentInvocation) ([]agent.AgentEvent, agent.AgentOutcome) {
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

func okStdout(result string) []byte {
	return []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"` + result + `"}` + "\n")
}

func TestLaunch_ReadOnlyAutonomy_NoFlag(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: okStdout("ok")}, []container.IOChunk{{Stream: "stdout", Data: okStdout("ok")}})
	a := droidAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "x", "autonomy": "read-only"}})
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	cmd := f.Calls[0].Run
	if strings.Contains(cmd, "--auto") || strings.Contains(cmd, "--skip-permissions-unsafe") {
		t.Errorf("autonomy=read-only must emit no autonomy flag: %s", cmd)
	}
}

func TestLaunch_TransportError_AgentLaunch(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// ExecResult.Err models a mid-stream backend transport fault (process launched,
	// then the exec died). Launch must surface it as retryable *agent.ErrAgentLaunch.
	f.ProgramExecAny(container.ExecResult{Err: errors.New("backend exec died mid-stream")}, nil)
	a := droidAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var launch *agent.ErrAgentLaunch
	if !errors.As(outcome.Err, &launch) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrAgentLaunch (transport fault)", outcome.Err)
	}
}

func TestLaunch_HappyPath_TypedOutput(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	line := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"{\"answer\":42}","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: line}, []container.IOChunk{{Stream: "stdout", Data: line}})

	a := droidAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: droid.AdapterRef,
		With:         ir.RawConfig{"prompt": "what is the answer"},
		OutputSchema: &ir.JSONSchema{"type": "object", "required": []string{"answer"}, "properties": map[string]any{"answer": map[string]any{"type": "integer"}}},
	}
	events, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if v, ok := outcome.Result.Output["answer"].(float64); !ok || v != 42 {
		t.Errorf("Output[answer] = %v", outcome.Result.Output["answer"])
	}
	if len(events) != 1 {
		t.Errorf("events = %d, want exactly 1 (the result envelope)", len(events))
	}
	if len(events) == 1 && !strings.Contains(string(events[0].Payload), `"num_turns":1`) {
		t.Errorf("event payload not the raw line: %s", events[0].Payload)
	}
}

func TestLaunch_CommandLine_Flags_And_OpsecEnv(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: okStdout("{\\\"ok\\\":true}")}, []container.IOChunk{{Stream: "stdout", Data: okStdout("{\\\"ok\\\":true}")}})

	a := droidAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: droid.AdapterRef,
		With: ir.RawConfig{
			"prompt": "do it", "model": "gpt-5.5", "reasoning_effort": "high", "autonomy": "high",
			"system_prompt": "be terse", "enabled_tools": []any{"Read", "Edit"}, "disabled_tools": []any{"Execute"},
		},
		IdempotencyKey: "idem-123",
		OutputSchema:   &ir.JSONSchema{"type": "object", "required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	cmd := f.Calls[0].Run
	for _, want := range []string{"droid exec", "-o json", "--model 'gpt-5.5'", "--reasoning-effort high", "--auto high", "--append-system-prompt 'be terse'", "--enabled-tools 'Read,Edit'", "--disabled-tools 'Execute'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q\nfull: %s", want, cmd)
		}
	}
	env := f.Calls[0].Env
	if env["FACTORY_API_KEY"] != "fk-test" {
		t.Errorf("FACTORY_API_KEY not forwarded: %v", env)
	}
	if env["OTEL_SDK_DISABLED"] != "true" || env["OTEL_CUSTOMER_ENABLED"] != "false" {
		t.Errorf("opsec telemetry env not injected: %v", env)
	}
	if env["AWF_IDEMPOTENCY_KEY"] != "idem-123" {
		t.Errorf("idempotency key not injected: %v", env)
	}
}

func TestLaunch_DefaultAutonomy_SkipPermissions(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: okStdout("ok")}, []container.IOChunk{{Stream: "stdout", Data: okStdout("ok")}})
	a := droidAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if !strings.Contains(f.Calls[0].Run, "--skip-permissions-unsafe") {
		t.Errorf("default autonomy should be --skip-permissions-unsafe: %s", f.Calls[0].Run)
	}
}

func TestLaunch_OmitsSessionFlags(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: okStdout("ok")}, []container.IOChunk{{Stream: "stdout", Data: okStdout("ok")}})
	a := droidAdapter(t, f)
	_, _ = drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	for _, forbidden := range []string{"--session-id", "--resume", "--fork", "-s ", " -r "} {
		if strings.Contains(f.Calls[0].Run, forbidden) {
			t.Errorf("command contained forbidden flag %q: %s", forbidden, f.Calls[0].Run)
		}
	}
}

func TestLaunch_FeedbackPrependedToPrompt(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: okStdout("ok")}, []container.IOChunk{{Stream: "stdout", Data: okStdout("ok")}})
	a := droidAdapter(t, f)
	inv := agent.AgentInvocation{NodePath: "gate[0].attempt-2.generate[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "fix it"}, Feedback: ir.RawConfig{"reason": "tests failed"}}
	_, _ = drainLaunch(t, a, h, inv)
	if !strings.Contains(f.Calls[0].Run, "previous verdict") || !strings.Contains(f.Calls[0].Run, "tests failed") {
		t.Errorf("feedback not prepended: %s", f.Calls[0].Run)
	}
}

func TestLaunch_ConfigErrorStderr_Permanent(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	stderr := []byte("Invalid model: nope-xyz\nAvailable built-in models: ...\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 1, Stdout: nil}, []container.IOChunk{{Stream: "stderr", Data: stderr}})
	a := droidAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var bad *agent.ErrInvalidConfig
	if !errors.As(outcome.Err, &bad) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrInvalidConfig (permanent config error)", outcome.Err)
	}
}

func TestLaunch_NoEnvelopeNoConfigPattern_UnexpectedExit(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	stderr := []byte("segfault: core dumped\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 139, Stdout: nil}, []container.IOChunk{{Stream: "stderr", Data: stderr}})
	a := droidAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var unexp *droid.ErrUnexpectedExit
	if !errors.As(outcome.Err, &unexp) {
		t.Fatalf("outcome.Err = %v, want *droid.ErrUnexpectedExit", outcome.Err)
	}
	if !strings.Contains(unexp.Stderr, "segfault") {
		t.Errorf("ErrUnexpectedExit.Stderr should carry the captured stderr: %q", unexp.Stderr)
	}
}

func TestLaunch_AuthFailure_Retryable_AgentLaunch(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	line := []byte(`{"type":"result","subtype":"failure","is_error":true,"num_turns":0,"result":"Authentication failed. set a valid FACTORY_API_KEY environment variable."}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 1, Stdout: line}, []container.IOChunk{{Stream: "stdout", Data: line}})
	a := droidAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var launch *agent.ErrAgentLaunch
	if !errors.As(outcome.Err, &launch) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrAgentLaunch (auth is retryable, not permanent)", outcome.Err)
	}
}

func TestLaunch_NilBackend(t *testing.T) {
	a, _ := droid.New(droid.WithEnv(map[string]string{"FACTORY_API_KEY": "fk-test"}))
	_, _, err := a.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{With: ir.RawConfig{"prompt": "x"}})
	var launch *agent.ErrAgentLaunch
	if !errors.As(err, &launch) {
		t.Fatalf("err = %v, want *agent.ErrAgentLaunch", err)
	}
}

func TestShellQuote_EscapesSingleQuotes(t *testing.T) {
	if got, want := droid.ShellQuoteForTest("it's"), `'it'\''s'`; got != want {
		t.Errorf("shellQuote = %q, want %q", got, want)
	}
}
