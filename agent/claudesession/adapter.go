package claudesession

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// stderrCaptureLimit bounds how many stderr bytes Launch accumulates for the
// unexpected-exit diagnostic (claude.ErrUnexpectedExit.Stderr). The error's
// own Error() truncates to ~200 chars for display; the raw field keeps a
// little headroom so callers inspecting the field see more context.
const stderrCaptureLimit = 8 << 10 // 8 KiB

// Adapter is the agent.Adapter implementation for Claude Code with deterministic
// session-id reuse. It wraps a *claude.Adapter for shared functionality
// (env passthrough, backend wiring, version, stream parsing) and adds:
//   - Capabilities().PersistentSession = true
//   - --session-id <deterministic-uuid> injected into every launch command
//   - --no-session-persistence is OMITTED (so the session journal is recorded
//     under the per-run CLAUDE_CONFIG_DIR the engine injects via inv.SessionConfigDir)
//
// The base *claude.Adapter is a named field (not embedded) used for delegation;
// Launch is overridden because the command construction differs (session-id flag,
// no --no-session-persistence). All other methods delegate to the base.
type Adapter struct {
	base    *claude.Adapter // shared env/backend/stream-parse logic
	backend container.Backend
	env     agent.SecretEnv
}

// Option configures the Adapter at construction time.
type Option func(*Adapter)

// WithEnv supplies the env-var allowlist forwarded into each `claude -p`
// invocation. Same semantics as claude.WithEnv.
func WithEnv(env map[string]string) Option {
	return func(a *Adapter) {
		if len(env) == 0 {
			a.env = agent.SecretEnv{}
		} else {
			out := make(agent.SecretEnv, len(env))
			for k, v := range env {
				out[k] = v
			}
			a.env = out
		}
	}
}

// WithBackend supplies the container.Backend used for Version and Launch.
func WithBackend(b container.Backend) Option {
	return func(a *Adapter) {
		a.backend = b
	}
}

// New constructs an Adapter. The base *claude.Adapter is constructed with
// the same env and backend options so that all shared logic (Version, stream
// parsing, env redaction) is delegated there.
func New(opts ...Option) (*Adapter, error) {
	a := &Adapter{
		env: agent.SecretEnv{},
	}
	for _, opt := range opts {
		opt(a)
	}
	// Construct the base claude adapter with the same env + backend so that
	// Version and other shared operations delegate there correctly.
	base, err := claude.New(
		claude.WithEnv(map[string]string(a.env)),
		claude.WithBackend(a.backend),
	)
	if err != nil {
		return nil, fmt.Errorf("agent/claudesession: construct base claude adapter: %w", err)
	}
	a.base = base
	return a, nil
}

// Ref returns the agent-runtime identifier.
func (*Adapter) Ref() string { return AdapterRef }

// Capabilities returns Caps{NativeSchema: true, PersistentSession: true}.
// Not Containerless — this adapter requires a container (like the base
// claude adapter). Gate independence is enforced by the engine's PR0a guard
// that rejects PersistentSession adapters in gate.evaluate.
func (*Adapter) Capabilities() agent.Caps {
	return agent.Caps{
		NativeSchema:      true,
		PersistentSession: true,
		IsolatedConfigDir: true,
	}
}

// RequiredEnv implements agent.CredentialNamer.
func (*Adapter) RequiredEnv() []string { return DefaultEnvAllowlist }

// Version delegates to the base claude adapter.
func (a *Adapter) Version(ctx context.Context, handle container.Handle) (string, error) {
	return a.base.Version(ctx, handle)
}

// ValidateConfig enforces the same with-schema as the claude adapter (it
// accepts the same keys), but ALLOWS session_id / continue / resume — those
// keys are managed internally by the adapter via the deterministic UUID, not
// by the workflow author. Unknown keys and missing prompt are still rejected.
//
// The difference from the base adapter: session reuse keys are NOT rejected
// (since this adapter IS the session adapter). We therefore reimplement
// validation rather than delegating to avoid the ErrSessionReuseAttempted
// path in the base.
func (a *Adapter) ValidateConfig(with ir.RawConfig) error {
	// 1. Unknown-key rejection (same allowed set as claude, minus the
	//    session-reuse rejection step).
	for _, k := range slices.Sorted(maps.Keys(with)) {
		if _, ok := allowedKeys[k]; !ok {
			return &agent.ErrInvalidConfig{
				Ref:        AdapterRef,
				Key:        k,
				Reason:     fmt.Sprintf("unknown with-key (allowed: %v)", slices.Sorted(maps.Keys(allowedKeys))),
				KeyUnknown: true,
			}
		}
	}

	// 2. prompt (required, string).
	prompt, ok := with[keyPrompt]
	if !ok {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: keyPrompt, Reason: "required"}
	}
	if _, ok := prompt.(string); !ok {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: keyPrompt, Reason: fmt.Sprintf("must be string, got %T", prompt)}
	}

	// 3. Optional-field types.
	if v, ok := with[keyModel]; ok {
		if _, ok := v.(string); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyModel)
		}
	}
	if v, ok := with[keyEffort]; ok {
		s, ok := v.(string)
		if !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keyEffort)
		}
		if !slices.Contains(effortValues, s) {
			return wrapInvalidConfig(fmt.Sprintf("must be one of %v, got %q", effortValues, s), keyEffort)
		}
	}
	if v, ok := with[keyMaxTurns]; ok {
		switch v.(type) {
		case int, int64, float64:
		default:
			return wrapInvalidConfig(fmt.Sprintf("must be integer, got %T", v), keyMaxTurns)
		}
	}
	if v, ok := with[keySystemPrompt]; ok {
		if _, ok := v.(string); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be string, got %T", v), keySystemPrompt)
		}
	}
	if v, ok := with[keyAllowedTools]; ok {
		if _, ok := v.([]any); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be array of strings, got %T", v), keyAllowedTools)
		}
	}
	if v, ok := with[keyBare]; ok {
		if _, ok := v.(bool); !ok {
			return wrapInvalidConfig(fmt.Sprintf("must be bool, got %T", v), keyBare)
		}
	}
	if v, ok := with[keyMaxBudgetUSD]; ok {
		switch v.(type) {
		case int, int64, float64:
		default:
			return wrapInvalidConfig(fmt.Sprintf("must be number, got %T", v), keyMaxBudgetUSD)
		}
	}

	// 4. bare-requires-API-key (same as claude adapter, decision 9).
	bare := defaultBare
	if v, ok := with[keyBare]; ok {
		bare = v.(bool)
	}
	if bare {
		_, haveAPIKey := a.env["ANTHROPIC_API_KEY"]
		_, haveAuthToken := a.env["ANTHROPIC_AUTH_TOKEN"]
		if !haveAPIKey && !haveAuthToken {
			return &ErrBareRequiresAPIKey{AvailableKeys: slices.Sorted(maps.Keys(a.env))}
		}
	}

	return nil
}

// Launch runs `claude -p ... --session-id <uuid>` inside handle.
// It uses the same streaming Backend.Exec / io.Pipe / bufio.Scanner pattern
// as the base claude.Adapter.Launch (copied here because the command
// construction differs and the base launch function is not exported).
//
// Key differences from base claude.Adapter.Launch:
//   - --session-id <sessionUUID(inv)> is appended
//   - --no-session-persistence is OMITTED (so Claude records the session)
func (a *Adapter) Launch(ctx context.Context, handle container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	if a.backend == nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: errors.New("agent/claudesession: Launch: no Backend wired (use WithBackend in New)")}
	}

	cmdString, err := assembleSessionCommand(inv)
	if err != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}

	// Build a fresh per-invocation env map from a.env so no code path ever
	// mutates the adapter's shared a.env (type-converting agent.SecretEnv to
	// map[string]string is a label change only — it aliases the same underlying
	// map). All writes below are safe because they target this local copy.
	env := make(map[string]string, len(a.env)+5)
	for k, v := range a.env {
		env[k] = v
	}
	if inv.IdempotencyKey != "" {
		env["AWF_IDEMPOTENCY_KEY"] = inv.IdempotencyKey
	}
	// Per-run config isolation (CLAUDE_CONFIG_DIR + per-run XDG state/cache) +
	// headless hygiene. Shared with the base claude adapter (KEEP IN SYNC).
	claude.ApplyPerRunConfigEnv(env, inv)
	execCmd := container.Cmd{
		Run: cmdString,
		Env: env,
	}

	chunks, resultCh, execErr := a.backend.Exec(ctx, handle, execCmd)
	if execErr != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: execErr}
	}

	events := make(chan agent.AgentEvent, 16)
	outcomeCh := make(chan agent.AgentOutcome, 1)

	pr, pw := io.Pipe()

	// stderrCh carries the stderr bytes captured by goroutine A so the
	// unexpected-exit path can populate claude.ErrUnexpectedExit.Stderr.
	// Buffered + closed-by-A so goroutine B reads it race-free after the
	// chunks channel has drained (the close establishes happens-before).
	stderrCh := make(chan string, 1)

	// Goroutine A: forward stdout chunks into pipe, accumulate stderr.
	go func() {
		defer func() { _ = pw.Close() }()
		// Bounded stderr capture: claude's stream-json result is stdout-only,
		// but on an early non-zero exit (no result event) the diagnostic lives
		// on stderr. Cap at stderrCaptureLimit so a noisy process can't grow
		// this unbounded.
		var stderrBuf []byte
		defer func() { stderrCh <- string(stderrBuf) }()
		for c := range chunks {
			if c.Stream != "stdout" {
				if c.Stream == "stderr" && len(stderrBuf) < stderrCaptureLimit {
					room := stderrCaptureLimit - len(stderrBuf)
					if len(c.Data) > room {
						stderrBuf = append(stderrBuf, c.Data[:room]...)
					} else {
						stderrBuf = append(stderrBuf, c.Data...)
					}
				}
				continue
			}
			if _, werr := pw.Write(c.Data); werr != nil {
				for range chunks {
				}
				return
			}
		}
	}()

	// Goroutine B: parse stream-json, emit events progressively, send outcome.
	go func() {
		defer close(outcomeCh)
		defer close(events)
		defer func() { _ = pr.Close() }()

		var capturedResult agent.AgentResult
		var kind string
		var captureErr error
		var initModel string

		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			msg, perr := claude.ParseStreamLine(line)
			if perr != nil {
				captureErr = perr
				continue
			}
			if msg.Type == "system" && msg.Subtype == "init" && msg.Model != "" {
				initModel = msg.Model
			}
			for _, ev := range claude.MessageToEvents(msg) {
				select {
				case events <- ev:
				case <-ctx.Done():
					outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
					return
				}
			}
			if msg.Type == "result" {
				res, eerr := claude.ExtractResult(msg, initModel)
				switch {
				case eerr == nil:
					capturedResult = res
					kind = "ok"
				case errors.Is(eerr, claude.ErrAuthFailureSentinel):
					kind = "auth"
					captureErr = eerr
				case strings.Contains(eerr.Error(), "structured_output"):
					kind = "unparseable"
				default:
					kind = "fatal"
					captureErr = eerr
				}
			}
		}
		if serr := scanner.Err(); serr != nil && kind == "" {
			kind = "fatal"
			captureErr = fmt.Errorf("agent/claudesession: scan stream-json: %w", serr)
		}

		execResult := <-resultCh

		switch {
		case execResult.Err != nil:
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: execResult.Err}}
		case kind == "ok":
			outcomeCh <- agent.AgentOutcome{Result: capturedResult}
		case kind == "unparseable":
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrUnparseableOutput{NodePath: inv.NodePath}}
		case kind == "auth":
			outcomeCh <- agent.AgentOutcome{Err: fmt.Errorf("agent/claudesession: authentication failed: %w: %w", agent.ErrPermissionDenied, captureErr)}
		case kind == "fatal":
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: captureErr}}
		default: // kind == "" — no result event was seen
			// stderrCh is closed-after-write by goroutine A; the receive blocks
			// only until that single value lands (after chunks drained), giving
			// a race-free read of the captured stderr.
			outcomeCh <- agent.AgentOutcome{Err: &claude.ErrUnexpectedExit{ExitCode: execResult.ExitCode, Stderr: <-stderrCh}}
		}
	}()

	return events, outcomeCh, nil
}

// assembleSessionCommand builds the full `claude -p ... --session-id <uuid>`
// shell-command string. It mirrors assembleCommand in the base claude package
// but:
//   - Appends --session-id <sessionUUID(inv)>
//   - Does NOT append --no-session-persistence
func assembleSessionCommand(inv agent.AgentInvocation) (string, error) {
	prompt, ok := inv.With[keyPrompt].(string)
	if !ok {
		return "", fmt.Errorf("agent/claudesession: assembleSessionCommand: with.prompt missing or non-string")
	}
	prompt, err := agent.PrependFeedback(prompt, inv.Feedback)
	if err != nil {
		return "", fmt.Errorf("agent/claudesession: prepend gate feedback: %w", err)
	}

	uuid := sessionUUID(inv)

	var parts []string
	parts = append(parts, "claude", "-p", shellQuote(prompt))
	parts = append(parts, "--output-format", "stream-json", "--verbose")
	// --no-session-persistence is intentionally OMITTED so the host journal
	// records the session for transcript capture/restore.
	//
	// Fresh turn (ResumeSession=false): --session-id <uuid> creates / adopts
	// the session by its deterministic id.
	// Restored turn (ResumeSession=true): --resume <uuid> re-primes Claude
	// from the transcript the engine wrote back before launching.
	// Exactly one flag is passed; both flags are never combined.
	if inv.ResumeSession {
		parts = append(parts, "--resume", uuid)
	} else {
		parts = append(parts, "--session-id", uuid)
	}

	if inv.OutputSchema != nil {
		schemaBytes, serr := json.Marshal(*inv.OutputSchema)
		if serr != nil {
			return "", fmt.Errorf("agent/claudesession: marshal OutputSchema: %w", serr)
		}
		parts = append(parts, "--json-schema", shellQuote(string(schemaBytes)))
	}

	bare := defaultBare
	if v, ok := inv.With[keyBare].(bool); ok {
		bare = v
	}
	if bare {
		parts = append(parts, "--bare")
	}
	if model, ok := inv.With[keyModel].(string); ok && model != "" {
		parts = append(parts, "--model", shellQuote(model))
	}
	if effort, ok := inv.With[keyEffort].(string); ok && effort != "" {
		parts = append(parts, "--effort", shellQuote(effort))
	}
	if mt, ok := toInt(inv.With[keyMaxTurns]); ok && mt > 0 {
		parts = append(parts, "--max-turns", fmt.Sprintf("%d", mt))
	}
	if sp, ok := inv.With[keySystemPrompt].(string); ok && sp != "" {
		parts = append(parts, "--system-prompt", shellQuote(sp))
	}
	if at, ok := inv.With[keyAllowedTools].([]any); ok && len(at) > 0 {
		var toolStrs []string
		for _, v := range at {
			if s, ok := v.(string); ok {
				toolStrs = append(toolStrs, s)
			}
		}
		if len(toolStrs) > 0 {
			parts = append(parts, "--allowedTools", shellQuote(strings.Join(toolStrs, ",")))
		}
	}
	if budget, ok := toFloat(inv.With[keyMaxBudgetUSD]); ok && budget > 0 {
		parts = append(parts, "--max-budget-usd", fmt.Sprintf("%g", budget))
	}

	return strings.Join(parts, " "), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	default:
		return 0, false
	}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	default:
		return 0, false
	}
}

// PreflightResume is a no-op for now — session restore is driven by the engine's
// content-addressed SessionRef (transcript blob), not a live provider session, so
// there is nothing to preflight at this stage. Real preflight logic (if any) lands
// with the live-integration task (M2d).
func (a *Adapter) PreflightResume(_ context.Context, _ agent.LiveResumePreflightRequest) error {
	return nil
}

// Compile-time assertions.
var _ agent.Adapter = (*Adapter)(nil)
var _ agent.ResumePreflighter = (*Adapter)(nil)
