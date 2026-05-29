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

// stream-json line helpers. droid emits one JSON event object per line; the
// adapter scans them progressively and emits one AgentEvent per line.
func sysLine() []byte {
	return []byte(`{"type":"system","subtype":"init","model":"claude-opus-4-8","session_id":"s1"}` + "\n")
}
func asstLine(text string) []byte {
	return []byte(`{"type":"message","role":"assistant","id":"m1","text":"` + text + `"}` + "\n")
}
func completionLine(finalText string) []byte {
	return []byte(`{"type":"completion","finalText":"` + finalText + `","numTurns":1,"durationMs":10,"session_id":"s1","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}` + "\n")
}

// chunk wraps a single stdout line as one IOChunk (so callers can stage the
// stream as separate chunks, exercising progressive multi-line parsing).
func chunk(b []byte) container.IOChunk { return container.IOChunk{Stream: "stdout", Data: b} }

func TestLaunch_ReadOnlyAutonomy_NoFlag(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(sysLine()), chunk(completionLine("ok"))})
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
	// SEPARATE IOChunks (one per line) → also demonstrates progressive multi-line parsing.
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{
		chunk(sysLine()),
		chunk(asstLine("working")),
		chunk(completionLine(`{\"answer\":42}`)),
	})

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
	if len(events) < 3 {
		t.Fatalf("events = %d, want >= 3 (system, message, completion)", len(events))
	}
	wantKinds := []string{"system", "message", "completion"}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Errorf("events[%d].Kind = %q, want %q", i, events[i].Kind, want)
		}
	}
	if !strings.Contains(string(events[2].Payload), "finalText") {
		t.Errorf("completion event payload not the raw line: %s", events[2].Payload)
	}
}

func TestLaunch_StreamsProgressively(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	toolCall := []byte(`{"type":"tool_call","id":"tc1","messageId":"m1","toolId":"t1","toolName":"Read","parameters":{"summary":"read a file"}}` + "\n")
	toolResult := []byte(`{"type":"tool_result","id":"tr1","messageId":"m1","toolId":"t1","isError":false,"value":"contents"}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{
		chunk(sysLine()),
		chunk(asstLine("let me look")),
		chunk(toolCall),
		chunk(toolResult),
		chunk(completionLine("done")),
	})
	a := droidAdapter(t, f)
	events, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	got := make([]string, len(events))
	for i, ev := range events {
		got[i] = ev.Kind
	}
	want := []string{"system", "message", "tool_call", "tool_result", "completion"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("event Kind sequence = %v, want %v (one event per line, in arrival order)", got, want)
	}
}

func TestLaunch_CommandLine_Flags_And_OpsecEnv(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(sysLine()), chunk(completionLine(`{\"ok\":true}`))})

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
	for _, want := range []string{"droid exec", "-o stream-json", "--model 'gpt-5.5'", "--reasoning-effort high", "--auto high", "--append-system-prompt 'be terse'", "--enabled-tools 'Read,Edit'", "--disabled-tools 'Execute'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q\nfull: %s", want, cmd)
		}
	}
	if strings.Contains(cmd, "-o json") {
		t.Errorf("command must use -o stream-json, not -o json: %s", cmd)
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
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(sysLine()), chunk(completionLine("ok"))})
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
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(sysLine()), chunk(completionLine("ok"))})
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
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(sysLine()), chunk(completionLine("ok"))})
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
	// No stdout events → kind "" → stderr pattern → permanent ErrInvalidConfig.
	stderr := []byte("Invalid model: nope-xyz\nAvailable built-in models: ...\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{{Stream: "stderr", Data: stderr}})
	a := droidAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var bad *agent.ErrInvalidConfig
	if !errors.As(outcome.Err, &bad) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrInvalidConfig (permanent config error)", outcome.Err)
	}
}

func TestLaunch_NoTerminalEventNoConfigPattern_UnexpectedExit(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// A system line but NO terminal completion/error event, plus an unrecognized
	// stderr → kind "" → ErrUnexpectedExit carrying the captured stderr.
	stderr := []byte("boom\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{chunk(sysLine()), {Stream: "stderr", Data: stderr}})
	a := droidAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: droid.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var unexp *droid.ErrUnexpectedExit
	if !errors.As(outcome.Err, &unexp) {
		t.Fatalf("outcome.Err = %v, want *droid.ErrUnexpectedExit", outcome.Err)
	}
	if !strings.Contains(unexp.Stderr, "boom") {
		t.Errorf("ErrUnexpectedExit.Stderr should carry the captured stderr: %q", unexp.Stderr)
	}
}

func TestLaunch_AuthFailure_Retryable_AgentLaunch(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	errLine := []byte(`{"type":"error","source":"cli","message":"Error: Authentication failed. set a valid FACTORY_API_KEY environment variable."}` + "\n")
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{chunk(sysLine()), chunk(errLine)})
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

func TestLaunch_FeedbackAndSchema_Compose(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(sysLine()), chunk(completionLine(`{\"ok\":true}`))})
	a := droidAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "gate[0].attempt-2.generate[0]", Uses: droid.AdapterRef,
		With:         ir.RawConfig{"prompt": "fix it"},
		Feedback:     ir.RawConfig{"reason": "tests failed"},
		OutputSchema: &ir.JSONSchema{"type": "object", "required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	cmd := f.Calls[0].Run
	verdictIdx := strings.Index(cmd, "previous verdict")
	schemaIdx := strings.Index(cmd, "JSON Schema")
	if verdictIdx < 0 || !strings.Contains(cmd, "tests failed") {
		t.Errorf("feedback not prepended: %s", cmd)
	}
	if schemaIdx < 0 {
		t.Errorf("schema directive not appended: %s", cmd)
	}
	if verdictIdx >= 0 && schemaIdx >= 0 && verdictIdx > schemaIdx {
		t.Errorf("expected feedback (verdict) BEFORE the schema directive; got verdict@%d schema@%d", verdictIdx, schemaIdx)
	}
}

func TestShellQuote_EscapesSingleQuotes(t *testing.T) {
	if got, want := droid.ShellQuoteForTest("it's"), `'it'\''s'`; got != want {
		t.Errorf("shellQuote = %q, want %q", got, want)
	}
}
