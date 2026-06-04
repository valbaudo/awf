package codex_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/codex"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

func codexLaunchAdapter(t *testing.T, f *container.Fake) *codex.Adapter {
	t.Helper()
	a, err := codex.New(codex.WithBackend(f), codex.WithEnv(map[string]string{"OPENAI_API_KEY": "sk-test", "CODEX_HOME": "/cdx"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func chunk(b []byte) container.IOChunk { return container.IOChunk{Stream: "stdout", Data: b} }
func nl(b []byte) []byte               { return append(append([]byte(nil), b...), '\n') }

func drainLaunch(t *testing.T, a *codex.Adapter, h container.Handle, inv agent.AgentInvocation) ([]agent.AgentEvent, agent.AgentOutcome) {
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

func TestLaunch_HappyPath_TypedOutput(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{
		chunk(nl(itemMsg(`{"answer":4}`))),
		chunk(nl(turnCompleted(20, 2, 5))),
	})
	a := codexLaunchAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: codex.AdapterRef,
		With:         ir.RawConfig{"prompt": "what is 2+2"},
		OutputSchema: &ir.JSONSchema{"type": "object", "additionalProperties": false, "required": []string{"answer"}, "properties": map[string]any{"answer": map[string]any{"type": "integer"}}},
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if v, ok := outcome.Result.Output["answer"].(float64); !ok || v != 4 {
		t.Errorf("Output[answer] = %v, want 4", outcome.Result.Output["answer"])
	}
	if outcome.Result.Metrics.Tokens.Input != 20 || outcome.Result.Metrics.Tokens.Output != 5 || outcome.Result.Metrics.Tokens.CacheReadInput != 2 {
		t.Errorf("tokens = %+v", outcome.Result.Metrics.Tokens)
	}
}

func TestLaunch_TwoAgentMessages_LastWins(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// premature answer, a command, then the FINAL answer → last-wins picks the 2nd.
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{
		chunk(nl(itemMsg(`{"answer":1}`))),
		chunk(nl(cmdStarted("ls"))),
		chunk(nl(cmdCompleted("ls", "a\nb\n", 0))),
		chunk(nl(itemMsg(`{"answer":4}`))),
		chunk(nl(turnCompleted(0, 0, 0))),
	})
	a := codexLaunchAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: codex.AdapterRef,
		With:         ir.RawConfig{"prompt": "count then answer"},
		OutputSchema: &ir.JSONSchema{"type": "object"},
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	if v, ok := outcome.Result.Output["answer"].(float64); !ok || v != 4 {
		t.Errorf("Output[answer] = %v, want 4 (last-wins, not the premature 1)", outcome.Result.Output["answer"])
	}
}

func TestLaunch_CommandLine_SchemaPrelude_Flags_Env(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(nl(itemMsg(`{"ok":true}`))), chunk(nl(turnCompleted(0, 0, 0)))})
	a := codexLaunchAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: codex.AdapterRef,
		With:           ir.RawConfig{"prompt": "do it", "model": "gpt-5-codex", "reasoning_effort": "high", "sandbox": "workspace-write"},
		OutputSchema:   &ir.JSONSchema{"type": "object"},
		IdempotencyKey: "idem-123",
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	cmd := f.Calls[0].Run
	for _, want := range []string{
		"printf '%s' '", "> /tmp/awf-codex-schema.json && ",
		"codex exec --json --skip-git-repo-check --ephemeral",
		"--output-schema /tmp/awf-codex-schema.json",
		"--sandbox 'workspace-write'", "-m 'gpt-5-codex'", "-c 'model_reasoning_effort=high'",
		"-- 'do it'", "</dev/null",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q\nfull: %s", want, cmd)
		}
	}
	// sandbox set → bypass flag MUST be absent.
	if strings.Contains(cmd, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("bypass flag present despite sandbox key: %s", cmd)
	}
	env := f.Calls[0].Env
	if env["OPENAI_API_KEY"] != "sk-test" || env["CODEX_HOME"] != "/cdx" {
		t.Errorf("auth env not forwarded: %v", env)
	}
	if env["AWF_IDEMPOTENCY_KEY"] != "idem-123" {
		t.Errorf("idempotency key not injected: %v", env)
	}
}

func TestLaunch_DefaultSandbox_BypassWhenUnset(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(nl(itemMsg("ok"))), chunk(nl(turnCompleted(0, 0, 0)))})
	a := codexLaunchAdapter(t, f)
	_, _ = drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	cmd := f.Calls[0].Run
	if !strings.Contains(cmd, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("default sandbox bypass missing: %s", cmd)
	}
	// No schema → no prelude, no --output-schema.
	if strings.Contains(cmd, "--output-schema") || strings.Contains(cmd, "printf") {
		t.Errorf("schema prelude emitted without OutputSchema: %s", cmd)
	}
}

func TestLaunch_OmitsSessionSubcommands(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(nl(itemMsg("ok"))), chunk(nl(turnCompleted(0, 0, 0)))})
	a := codexLaunchAdapter(t, f)
	_, _ = drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	cmd := f.Calls[0].Run
	// Forbidden reuse tokens. NOTE: --sandbox/-s and the bypass flag are EXPECTED —
	// do NOT add a bare " -s " check (it collides with the sandbox flag).
	for _, forbidden := range []string{" resume", " fork", "--last", "session_id", " continue"} {
		if strings.Contains(cmd, forbidden) {
			t.Errorf("command contained forbidden reuse token %q: %s", forbidden, cmd)
		}
	}
}

func TestLaunch_PermanentInvalidRequest(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{
		chunk(nl(errorEvent(apiErr400))),
		chunk(nl(turnFailed(apiErr400))),
	})
	a := codexLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var bad *agent.ErrInvalidConfig
	if !errors.As(outcome.Err, &bad) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrInvalidConfig (permanent)", outcome.Err)
	}
}

func TestLaunch_RetryableRateLimit(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// 429 body embeds the token "invalid_request_error" as a decoy → must stay retryable.
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{chunk(nl(turnFailed(apiErr429)))})
	a := codexLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var launch *agent.ErrAgentLaunch
	if !errors.As(outcome.Err, &launch) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrAgentLaunch (retryable)", outcome.Err)
	}
}

func TestLaunch_TransientError_ThenSuccess(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// A NON-FATAL `error` event (a reconnect notice) followed by a successful turn:
	// the good result MUST win — a bare `error` is not a verdict (engine: crash≠verdict).
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{
		chunk(nl(errorEvent("reconnecting 1/5"))),
		chunk(nl(itemMsg(`{"answer":4}`))),
		chunk(nl(turnCompleted(0, 0, 0))),
	})
	a := codexLaunchAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: codex.AdapterRef,
		With:         ir.RawConfig{"prompt": "x"},
		OutputSchema: &ir.JSONSchema{"type": "object"},
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("transient error before turn.completed must NOT fail the run: %v", outcome.Err)
	}
	if v, ok := outcome.Result.Output["answer"].(float64); !ok || v != 4 {
		t.Errorf("Output[answer] = %v, want 4", outcome.Result.Output["answer"])
	}
}

func TestLaunch_NoAgentMessage_UnexpectedExit(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(nl(turnCompleted(0, 0, 0)))})
	a := codexLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var unexp *codex.ErrUnexpectedExit
	if !errors.As(outcome.Err, &unexp) {
		t.Fatalf("outcome.Err = %v, want *codex.ErrUnexpectedExit", outcome.Err)
	}
}

func TestLaunch_NoTerminal_UnexpectedExit(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{chunk(nl(itemMsg("partial")))})
	a := codexLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var unexp *codex.ErrUnexpectedExit
	if !errors.As(outcome.Err, &unexp) {
		t.Fatalf("outcome.Err = %v, want *codex.ErrUnexpectedExit (no terminal event)", outcome.Err)
	}
}

func TestLaunch_TransportError_AgentLaunch(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{Err: errors.New("backend exec died")}, nil)
	a := codexLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var launch *agent.ErrAgentLaunch
	if !errors.As(outcome.Err, &launch) {
		t.Fatalf("outcome.Err = %v, want *agent.ErrAgentLaunch", outcome.Err)
	}
}

func TestLaunch_FeedbackPrepended(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(nl(itemMsg(`{"ok":true}`))), chunk(nl(turnCompleted(0, 0, 0)))})
	a := codexLaunchAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "gate[0].attempt-2.generate[0]", Uses: codex.AdapterRef,
		With:         ir.RawConfig{"prompt": "fix it"},
		Feedback:     ir.RawConfig{"reason": "tests failed"},
		OutputSchema: &ir.JSONSchema{"type": "object"},
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	cmd := f.Calls[0].Run
	if !strings.Contains(cmd, "previous verdict") || !strings.Contains(cmd, "tests failed") {
		t.Errorf("feedback not prepended: %s", cmd)
	}
}

func TestLaunch_NilBackend(t *testing.T) {
	a, _ := codex.New()
	_, _, err := a.Launch(context.Background(), container.Handle{Name: "lab"}, agent.AgentInvocation{With: ir.RawConfig{"prompt": "x"}})
	var launch *agent.ErrAgentLaunch
	if !errors.As(err, &launch) {
		t.Fatalf("err = %v, want *agent.ErrAgentLaunch", err)
	}
}

func TestShellQuote_EscapesSingleQuotes(t *testing.T) {
	if got, want := codex.ShellQuoteForTest("it's"), `'it'\''s'`; got != want {
		t.Errorf("shellQuote = %q, want %q", got, want)
	}
}

// TestAssembleCommand_EscapesAdversarialInput locks the sh -c quoting correctness
// (NOT model-level prompt injection — that's the container's job). AWF is an
// offensive tool: prompts and schema descriptions routinely carry shell
// metacharacters, so the assembled command MUST single-quote them and keep the
// printf format literal. Guards against a future refactor that drops shellQuote or
// moves the schema into the printf format position.
func TestAssembleCommand_EscapesAdversarialInput(t *testing.T) {
	schema := &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"x"},
		"properties": map[string]any{
			"x": map[string]any{"type": "string", "description": `it's a 50% trap`},
		},
	}
	prompt := `'; rm -rf / # $(touch pwned)`
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: codex.AdapterRef,
		With:         ir.RawConfig{"prompt": prompt},
		OutputSchema: schema,
	}
	cmd, err := codex.AssembleCommandForTest(inv)
	if err != nil {
		t.Fatalf("assembleCommand: %v", err)
	}
	// The adversarial prompt must ride as ONE POSIX-single-quoted argv element.
	if want := "-- " + codex.ShellQuoteForTest(prompt); !strings.Contains(cmd, want) {
		t.Errorf("prompt not single-quote-escaped (want %q):\n%s", want, cmd)
	}
	// The single quote in the schema description must be escaped as '\'' .
	if !strings.Contains(cmd, `it'\''s a 50% trap`) {
		t.Errorf("schema single-quote not escaped:\n%s", cmd)
	}
	// The printf FORMAT must stay the literal '%s' — schema bytes never reach the
	// format position, so a `%` in the schema can't corrupt the write.
	if !strings.Contains(cmd, "printf '%s' '") {
		t.Errorf("printf format not literal '%%s':\n%s", cmd)
	}
	// "</dev/null" must be the LAST token (a shell redirect, never an argument).
	if !strings.HasSuffix(cmd, " </dev/null") {
		t.Errorf("command must end with the </dev/null redirect:\n%s", cmd)
	}
}
