package codex_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

var codexSchemaPathRE = regexp.MustCompile(`/tmp/awf-codex-schema-[0-9a-f]{64}\.json`)

func schemaPathFromCommand(t *testing.T, cmd string) string {
	t.Helper()
	paths := codexSchemaPathRE.FindAllString(cmd, -1)
	if len(paths) != 2 {
		t.Fatalf("command has %d schema-path occurrences, want write + flag:\n%s", len(paths), cmd)
	}
	if paths[0] != paths[1] {
		t.Fatalf("schema write path %q != --output-schema path %q:\n%s", paths[0], paths[1], cmd)
	}
	return paths[0]
}

type unresolvedStagingBackend struct {
	*container.Fake
}

func (b *unresolvedStagingBackend) Capabilities() container.Caps {
	caps := b.Fake.Capabilities()
	caps.StagingRoot = ".awf"
	return caps
}

type relativeStagingBackend struct {
	*container.Fake
	workdir string
}

func (b *relativeStagingBackend) Capabilities() container.Caps {
	caps := b.Fake.Capabilities()
	caps.StagingRoot = ".awf"
	return caps
}

func (b *relativeStagingBackend) ResolveWorkdirPath(_ container.Handle, rel string) string {
	return filepath.Join(b.workdir, rel)
}

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

func TestLaunch_InvocationCodexHomesAreDistinctDeterministicAndOpaque(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{
		chunk(nl(itemMsg("ok"))),
		chunk(nl(turnCompleted(0, 0, 0))),
	})
	const (
		baseHome = "/runner/base-codex-home"
		apiKey   = "sk-do-not-put-this-in-the-command"
	)
	a, err := codex.New(codex.WithBackend(f), codex.WithEnv(map[string]string{
		"CODEX_HOME":     baseHome,
		"OPENAI_API_KEY": apiKey,
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	inv := agent.AgentInvocation{
		NodePath:       "map[0].item/raw node;$(unsafe).graph[0]",
		Uses:           codex.AdapterRef,
		RunContext:     agent.RunContext{RunID: "run/raw secret id"},
		With:           ir.RawConfig{"prompt": "x"},
		IdempotencyKey: "idem/raw secret key",
		Attempt:        1,
	}
	_, first := drainLaunch(t, a, h, inv)
	if first.Err != nil {
		t.Fatalf("first outcome: %v", first.Err)
	}

	inv.Attempt = 2 // retries of the same logical invocation intentionally reuse the home
	_, retry := drainLaunch(t, a, h, inv)
	if retry.Err != nil {
		t.Fatalf("retry outcome: %v", retry.Err)
	}

	invOtherNode := inv
	invOtherNode.NodePath = "map[0].item/other.graph[0]"
	_, _ = drainLaunch(t, a, h, invOtherNode)
	invOtherKey := inv
	invOtherKey.IdempotencyKey = "idem/other"
	_, _ = drainLaunch(t, a, h, invOtherKey)
	invOtherRun := inv
	invOtherRun.RunContext.RunID = "run/other"
	_, _ = drainLaunch(t, a, h, invOtherRun)

	if len(f.Calls) != 5 {
		t.Fatalf("Exec calls = %d, want 5", len(f.Calls))
	}
	rawIdentitiesAndSecrets := []string{
		inv.RunContext.RunID, inv.NodePath, inv.IdempotencyKey,
		invOtherNode.NodePath, invOtherKey.IdempotencyKey, invOtherRun.RunContext.RunID,
		apiKey,
	}
	homes := make([]string, len(f.Calls))
	for i, call := range f.Calls {
		home := call.Env["CODEX_HOME"]
		homes[i] = home
		if !filepath.IsAbs(home) {
			t.Errorf("call %d CODEX_HOME = %q, want absolute", i, home)
		}
		if matched, _ := regexp.MatchString(`^/work/\.awf/codex-home/[0-9a-f]{64}$`, home); !matched {
			t.Errorf("call %d CODEX_HOME = %q, want opaque path below staging root", i, home)
		}
		for _, raw := range rawIdentitiesAndSecrets {
			if strings.Contains(home, raw) || strings.Contains(call.Run, raw) {
				t.Errorf("call %d leaked raw identity/secret %q in home or command\nhome: %s\ncommand: %s", i, raw, home, call.Run)
			}
		}
		if got := call.Env["AWF_CODEX_BASE_HOME"]; got != baseHome {
			t.Errorf("call %d AWF_CODEX_BASE_HOME = %q, want %q", i, got, baseHome)
		}
		if strings.Contains(call.Run, baseHome) {
			t.Errorf("call %d embedded base home in command instead of passing it via env: %s", i, call.Run)
		}
	}
	if homes[0] != homes[1] {
		t.Errorf("retry CODEX_HOME changed: first %q, retry %q", homes[0], homes[1])
	}
	for i := 2; i < len(homes); i++ {
		if homes[i] == homes[0] {
			t.Errorf("identity variant call %d shared CODEX_HOME %q with first invocation", i, homes[i])
		}
	}

	cmd := f.Calls[0].Run
	for _, want := range []string{
		`umask 077`,
		`for awf_codex_file in auth.json config.toml`,
		`cp "$awf_codex_base/$awf_codex_file" "$CODEX_HOME/$awf_codex_file"`,
		`unset AWF_CODEX_BASE_HOME`,
		`[ -f "$CODEX_HOME/auth.json" ]`,
		`printenv OPENAI_API_KEY | codex login --with-api-key`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("isolated-home setup missing %q\ncommand: %s", want, cmd)
		}
	}
}

func TestLaunch_RelativeStagingRootResolvesToAbsoluteCodexHome(t *testing.T) {
	f := container.NewFake()
	workdir := t.TempDir()
	b := &relativeStagingBackend{Fake: f, workdir: workdir}
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{
		chunk(nl(itemMsg("ok"))),
		chunk(nl(turnCompleted(0, 0, 0))),
	})
	a, err := codex.New(codex.WithBackend(b))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	inv := agent.AgentInvocation{
		NodePath:   "graph[0]",
		Uses:       codex.AdapterRef,
		RunContext: agent.RunContext{RunID: "run-1"},
		With:       ir.RawConfig{"prompt": "x"},
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome: %v", outcome.Err)
	}
	home := f.Calls[0].Env["CODEX_HOME"]
	if base, ok := f.Calls[0].Env["AWF_CODEX_BASE_HOME"]; !ok || base != "" {
		t.Errorf("default base helper = %q (present %v), want explicit empty override", base, ok)
	}
	if !filepath.IsAbs(home) {
		t.Fatalf("CODEX_HOME = %q, want absolute", home)
	}
	wantParent := filepath.Join(workdir, ".awf", "codex-home") + string(filepath.Separator)
	if !strings.HasPrefix(home, wantParent) {
		t.Errorf("CODEX_HOME = %q, want under %q", home, wantParent)
	}
}

func TestLaunch_RelativeStagingRootWithoutResolverFailsClosed(t *testing.T) {
	f := container.NewFake()
	b := &unresolvedStagingBackend{Fake: f}
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	a, err := codex.New(codex.WithBackend(b))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = a.Launch(context.Background(), h, agent.AgentInvocation{
		NodePath:   "graph[0]",
		Uses:       codex.AdapterRef,
		RunContext: agent.RunContext{RunID: "run-1"},
		With:       ir.RawConfig{"prompt": "x"},
	})
	var launchErr *agent.ErrAgentLaunch
	if !errors.As(err, &launchErr) || !strings.Contains(err.Error(), "WorkdirResolver") {
		t.Fatalf("Launch error = %v, want ErrAgentLaunch explaining missing WorkdirResolver", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("backend Exec called despite unresolved relative CODEX_HOME: %+v", f.Calls)
	}
}

func TestLaunch_CommandLine_SchemaPrelude_Flags_Env(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{chunk(nl(itemMsg(`{"ok":true}`))), chunk(nl(turnCompleted(0, 0, 0)))})
	a := codexLaunchAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: codex.AdapterRef,
		With:           ir.RawConfig{"prompt": "do it", "model": "gpt-5-codex", "effort": "high", "sandbox": "workspace-write"},
		OutputSchema:   &ir.JSONSchema{"type": "object"},
		IdempotencyKey: "idem-123",
	}
	_, outcome := drainLaunch(t, a, h, inv)
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v", outcome.Err)
	}
	cmd := f.Calls[0].Run
	for _, want := range []string{
		"codex login --with-api-key", "printf '%s' '",
		"codex exec --json --skip-git-repo-check --ephemeral",
		"--sandbox 'workspace-write'", "-m 'gpt-5-codex'", "-c 'model_reasoning_effort=high'",
		"-- 'do it'", "</dev/null",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q\nfull: %s", want, cmd)
		}
	}
	schemaPath := schemaPathFromCommand(t, cmd)
	if !strings.Contains(cmd, "> "+schemaPath+" &&") || !strings.Contains(cmd, "--output-schema "+schemaPath) {
		t.Errorf("schema path not wired to both write and flag:\n%s", cmd)
	}
	// sandbox set → bypass flag MUST be absent.
	if strings.Contains(cmd, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("bypass flag present despite sandbox key: %s", cmd)
	}
	env := f.Calls[0].Env
	if env["OPENAI_API_KEY"] != "sk-test" || env["AWF_CODEX_BASE_HOME"] != "/cdx" {
		t.Errorf("auth/base env not forwarded: %v", env)
	}
	if matched, _ := regexp.MatchString(`^/work/\.awf/codex-home/[0-9a-f]{64}$`, env["CODEX_HOME"]); !matched {
		t.Errorf("isolated CODEX_HOME = %q, want opaque staging path", env["CODEX_HOME"])
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
	// Every launch has the isolated-home setup, but no schema means no schema
	// printf and no --output-schema flag.
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
	cmd, err := codex.AssembleCommandForTest(inv, false)
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

// A bare terminal `error` event (codex's "unrecoverable error emitted directly by
// the event stream") with NO turn.failed/turn.completed classifies via
// isPermanentCodexError, exactly like a turn.failed.

func TestLaunch_BareTerminalError_Permanent(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{chunk(nl(errorEvent(apiErr400)))})
	a := codexLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var bad *agent.ErrInvalidConfig
	if !errors.As(outcome.Err, &bad) {
		t.Fatalf("bare terminal error (400) = %v, want *agent.ErrInvalidConfig (permanent)", outcome.Err)
	}
}

func TestLaunch_BareTerminalError_Retryable(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// 429 body embeds "invalid_request_error" as a decoy → must stay retryable.
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{chunk(nl(errorEvent(apiErr429)))})
	a := codexLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var launch *agent.ErrAgentLaunch
	if !errors.As(outcome.Err, &launch) {
		t.Fatalf("bare terminal error (429) = %v, want *agent.ErrAgentLaunch (retryable)", outcome.Err)
	}
}

func TestLaunch_SchemaUnparseable_ThroughLaunch(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// turn.completed + a non-JSON agent_message UNDER a schema → strict unmarshal
	// fails end-to-end through Launch → ErrUnparseableOutput (retryable → gate repair).
	f.ProgramExecAny(container.ExecResult{ExitCode: 0}, []container.IOChunk{
		chunk(nl(itemMsg("not json at all"))),
		chunk(nl(turnCompleted(0, 0, 0))),
	})
	a := codexLaunchAdapter(t, f)
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: codex.AdapterRef,
		With:         ir.RawConfig{"prompt": "x"},
		OutputSchema: &ir.JSONSchema{"type": "object"},
	}
	_, outcome := drainLaunch(t, a, h, inv)
	var unp *agent.ErrUnparseableOutput
	if !errors.As(outcome.Err, &unp) {
		t.Fatalf("non-JSON agent_message under schema = %v, want *agent.ErrUnparseableOutput", outcome.Err)
	}
}

func TestLaunch_TurnFailedEmptyMessage_FallsBackToErrorEvent(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// codex's wire pattern is error→turn.failed; when turn.failed carries an EMPTY
	// message, classification must fall back to the preceding `error` event text.
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{
		chunk(nl(errorEvent(apiErr400))),
		chunk(nl(turnFailed(""))),
	})
	a := codexLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var bad *agent.ErrInvalidConfig
	if !errors.As(outcome.Err, &bad) {
		t.Fatalf("turn.failed(empty)+error(400) = %v, want *agent.ErrInvalidConfig (fallback to error event)", outcome.Err)
	}
}

func TestLaunch_TurnFailedNoMessage_UnexpectedExit(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// turn.failed with no message AND no preceding error event → ErrUnexpectedExit
	// (carries the exit code) rather than a content-free "codex turn failed:".
	f.ProgramExecAny(container.ExecResult{ExitCode: 1}, []container.IOChunk{chunk(nl(turnFailed("")))})
	a := codexLaunchAdapter(t, f)
	_, outcome := drainLaunch(t, a, h, agent.AgentInvocation{NodePath: "graph[0]", Uses: codex.AdapterRef, With: ir.RawConfig{"prompt": "x"}})
	var unexp *codex.ErrUnexpectedExit
	if !errors.As(outcome.Err, &unexp) {
		t.Fatalf("turn.failed(empty, no error event) = %v, want *codex.ErrUnexpectedExit", outcome.Err)
	}
}

// codex 0.146.0 does not honor a bare OPENAI_API_KEY env var for exec auth
// (401 "Missing bearer", proven against the binary on 2026-08-16): the key must
// be materialized into auth.json via `codex login --with-api-key` first. The
// isolated-home setup always runs; login runs only when the adapter's env carries
// the key and the copied base home had no auth.json. Login chatter goes to stderr
// so codex --json retains exclusive ownership of stdout.
func TestAssembleCommand_APIKeyLoginPrelude(t *testing.T) {
	inv := agent.AgentInvocation{
		NodePath: "graph[0]", Uses: codex.AdapterRef,
		With: ir.RawConfig{"prompt": "hi"},
	}

	withKey, err := codex.AssembleCommandForTest(inv, true)
	if err != nil {
		t.Fatalf("assembleCommand: %v", err)
	}
	if !strings.HasPrefix(withKey, `umask 077; mkdir -p "$CODEX_HOME"`) {
		t.Errorf("command must start with isolated-home setup:\n%s", withKey)
	}
	copyAt := strings.Index(withKey, `cp "$awf_codex_base/$awf_codex_file"`)
	loginAt := strings.Index(withKey, "codex login --with-api-key")
	execAt := strings.Index(withKey, "codex exec --json")
	if copyAt < 0 || loginAt < copyAt || execAt < loginAt {
		t.Errorf("base copy, login, and exec are missing or out of order:\n%s", withKey)
	}

	withoutKey, err := codex.AssembleCommandForTest(inv, false)
	if err != nil {
		t.Fatalf("assembleCommand: %v", err)
	}
	if strings.Contains(withoutKey, "codex login") {
		t.Errorf("no API key in env → no login prelude:\n%s", withoutKey)
	}
}

func TestAssembleCommand_APIKeyAndOutputSchemaCombinePreludes(t *testing.T) {
	inv := agent.AgentInvocation{
		NodePath:       "graph[0]",
		Uses:           codex.AdapterRef,
		With:           ir.RawConfig{"prompt": "hi"},
		OutputSchema:   &ir.JSONSchema{"type": "object"},
		IdempotencyKey: "typed-step",
	}

	cmd, err := codex.AssembleCommandForTest(inv, true)
	if err != nil {
		t.Fatalf("assembleCommand: %v", err)
	}
	copyAt := strings.Index(cmd, `cp "$awf_codex_base/$awf_codex_file"`)
	loginAt := strings.Index(cmd, "codex login --with-api-key")
	schemaAt := strings.Index(cmd, "printf '%s'")
	execAt := strings.Index(cmd, "codex exec --json")
	if copyAt < 0 || loginAt < copyAt || schemaAt < loginAt || execAt < schemaAt {
		t.Fatalf("home copy, login, schema, and exec must appear in that order:\n%s", cmd)
	}
	schemaPath := schemaPathFromCommand(t, cmd)
	if !strings.Contains(cmd, "> "+schemaPath+" && codex exec --json") {
		t.Errorf("schema prelude must feed the following codex exec:\n%s", cmd)
	}
}

func TestAssembleCommand_CopiesBaseAuthAndConfigBeforeTypedAuthenticatedExec(t *testing.T) {
	baseHome := t.TempDir()
	isolatedHome := filepath.Join(t.TempDir(), "isolated")
	binDir := t.TempDir()
	const (
		authValue   = `{"auth":"auth-secret-value"}`
		configValue = "provider_token = \"config-secret-value\"\n"
		apiKey      = "sk-api-secret-value"
	)
	for name, value := range map[string]string{
		"auth.json":   authValue,
		"config.toml": configValue,
	} {
		if err := os.WriteFile(filepath.Join(baseHome, name), []byte(value), 0o600); err != nil {
			t.Fatalf("write base %s: %v", name, err)
		}
	}
	codexBin := filepath.Join(binDir, "codex")
	codexScript := `#!/bin/sh
case "$1" in
  login) echo "unexpected login: copied auth.json was not visible" >&2; exit 91 ;;
  exec)
    printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
    printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0}}'
    ;;
  *) exit 92 ;;
esac
`
	if err := os.WriteFile(codexBin, []byte(codexScript), 0o700); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	inv := agent.AgentInvocation{
		NodePath:       "node/raw-private-id",
		Uses:           codex.AdapterRef,
		RunContext:     agent.RunContext{RunID: "run/raw-private-id"},
		With:           ir.RawConfig{"prompt": "hi"},
		OutputSchema:   &ir.JSONSchema{"type": "object"},
		IdempotencyKey: "idem/raw-private-id",
	}
	command, err := codex.AssembleCommandForTest(inv, true)
	if err != nil {
		t.Fatalf("assembleCommand: %v", err)
	}
	schemaPath := schemaPathFromCommand(t, command)
	t.Cleanup(func() { _ = os.Remove(schemaPath) })
	for _, forbidden := range []string{baseHome, isolatedHome, authValue, configValue, apiKey, inv.RunContext.RunID, inv.NodePath, inv.IdempotencyKey} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("command exposed path, identity, or secret %q:\n%s", forbidden, command)
		}
	}

	sh := exec.Command("sh", "-c", command)
	sh.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"CODEX_HOME=" + isolatedHome,
		"AWF_CODEX_BASE_HOME=" + baseHome,
		"OPENAI_API_KEY=" + apiKey,
	}
	output, err := sh.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated setup + typed authenticated exec: %v\noutput: %s\ncommand: %s", err, output, command)
	}
	for _, secret := range []string{authValue, configValue, apiKey} {
		if strings.Contains(string(output), secret) {
			t.Errorf("command output exposed secret %q: %s", secret, output)
		}
	}
	for name, want := range map[string]string{
		"auth.json":   authValue,
		"config.toml": configValue,
	} {
		path := filepath.Join(isolatedHome, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read copied %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("copied %s = %q, want exact base bytes %q", name, got, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat copied %s: %v", name, err)
		}
		if gotMode := info.Mode().Perm(); gotMode != 0o600 {
			t.Errorf("copied %s mode = %#o, want 0600", name, gotMode)
		}
	}
}

func TestAssembleCommand_ParallelInvocationsUseDistinctSchemaPaths(t *testing.T) {
	schema := &ir.JSONSchema{"type": "object"}
	invA := agent.AgentInvocation{
		NodePath:       "map[0].item-0.graph[0]",
		Uses:           codex.AdapterRef,
		RunContext:     agent.RunContext{RunID: "run/raw-a"},
		With:           ir.RawConfig{"prompt": "x"},
		OutputSchema:   schema,
		IdempotencyKey: "parallel/a with spaces",
	}
	invB := invA
	invB.NodePath = "map[0].item-1.graph[0]"
	invB.IdempotencyKey = "parallel/b;$(unsafe)"

	cmdA, err := codex.AssembleCommandForTest(invA, false)
	if err != nil {
		t.Fatalf("assembleCommand(invA): %v", err)
	}
	cmdB, err := codex.AssembleCommandForTest(invB, false)
	if err != nil {
		t.Fatalf("assembleCommand(invB): %v", err)
	}
	pathA := schemaPathFromCommand(t, cmdA)
	pathB := schemaPathFromCommand(t, cmdB)
	if pathA == pathB {
		t.Fatalf("parallel invocations share schema path %q", pathA)
	}
	for _, raw := range []string{invA.RunContext.RunID, invA.NodePath, invA.IdempotencyKey, invB.NodePath, invB.IdempotencyKey} {
		if strings.Contains(cmdA, raw) || strings.Contains(cmdB, raw) {
			t.Fatalf("raw invocation identity %q leaked into a schema command:\nA: %s\nB: %s", raw, cmdA, cmdB)
		}
	}

	invA.Attempt = 2
	cmdARepeat, err := codex.AssembleCommandForTest(invA, false)
	if err != nil {
		t.Fatalf("assembleCommand(invA repeat): %v", err)
	}
	if repeatPath := schemaPathFromCommand(t, cmdARepeat); repeatPath != pathA {
		t.Errorf("same invocation schema path changed: first %q, repeat %q", pathA, repeatPath)
	}

	invOtherRun := invA
	invOtherRun.RunContext.RunID = "run/raw-b"
	cmdOtherRun, err := codex.AssembleCommandForTest(invOtherRun, false)
	if err != nil {
		t.Fatalf("assembleCommand(other run): %v", err)
	}
	if otherRunPath := schemaPathFromCommand(t, cmdOtherRun); otherRunPath == pathA {
		t.Errorf("different runs share schema path %q", pathA)
	}
	if strings.Contains(cmdOtherRun, invOtherRun.RunContext.RunID) {
		t.Errorf("raw run ID leaked into schema command: %s", cmdOtherRun)
	}
}
