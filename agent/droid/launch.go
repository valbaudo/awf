package droid

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
)

// Launch runs `droid exec -o stream-json ...` inside handle via the streaming
// Backend.Exec. -o stream-json emits NDJSON flushed LIVE (verified v0.138.0:
// a system/init line, message lines, tool_call/tool_result lines, then a
// terminal "completion" — or a terminal "error"). Launch scans those lines and
// emits ONE AgentEvent per line as it arrives (the realtime tap renders these
// live), capturing the terminal completion/error for the AgentOutcome.
// γ contract: returns IMMEDIATELY with both channels open; events closes BEFORE
// outcome (defer LIFO); never reuses a session.
func (a *Adapter) Launch(ctx context.Context, handle container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	if a.backend == nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: errors.New("agent/droid: Launch: no Backend wired (use WithBackend in New)")}
	}
	cmdString, err := assembleCommand(inv)
	if err != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}
	env := make(map[string]string, len(a.env)+3)
	for k, v := range a.env {
		env[k] = v
	}
	// Opsec (verified, droid v0.138.0): the CLI's OTel tracer is hardcoded-on and
	// exports to telemetry.factory.ai unless OTEL_SDK_DISABLED=true; the
	// customer-export path is gated by OTEL_CUSTOMER_ENABLED. (FACTORY_OTEL_ENABLED
	// is NOT read by the CLI.) cloudSessionSync has no env knob — disable it
	// operationally in the image (see man/awf.1.md).
	env["OTEL_SDK_DISABLED"] = "true"
	env["OTEL_CUSTOMER_ENABLED"] = "false"
	if inv.IdempotencyKey != "" {
		env["AWF_IDEMPOTENCY_KEY"] = inv.IdempotencyKey
	}
	// BYOK with tls_insecure: droid is Bun/Node-based, so NODE_TLS_REJECT_UNAUTHORIZED=0
	// is the only TLS-skip lever. The api_key_env-named var is already forwarded (copied
	// from a.env above; validation guaranteed its presence).
	if insecure, _ := inv.With[keyTLSInsecure].(bool); insecure {
		env["NODE_TLS_REJECT_UNAUTHORIZED"] = "0"
	}

	chunks, resultCh, execErr := a.backend.Exec(ctx, handle, container.Cmd{Run: cmdString, Env: env})
	if execErr != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: execErr}
	}

	// γ contract: both channels returned OPEN; events emitted progressively.
	events := make(chan agent.AgentEvent, 16)
	outcomeCh := make(chan agent.AgentOutcome, 1)
	pr, pw := io.Pipe()
	stderrCh := make(chan string, 1)

	// Goroutine A: forward stdout chunks into the pipe (for line scanning) and
	// accumulate stderr (for the no-terminal-event config-error path). Hands the
	// stderr off on a buffered channel after closing the pipe writer.
	go func() {
		var stderr bytes.Buffer
		defer func() {
			_ = pw.Close()
			stderrCh <- stderr.String()
		}()
		for c := range chunks {
			switch c.Stream {
			case "stdout":
				if _, werr := pw.Write(c.Data); werr != nil {
					for range chunks { // reader closed early; drain so the backend can finish
					}
					return
				}
			case "stderr":
				stderr.Write(c.Data)
			}
		}
	}()

	// Goroutine B: scan stdout lines, emit one AgentEvent per line PROGRESSIVELY
	// (this is the realtime path), capture the terminal completion/error, then
	// send exactly one AgentOutcome. defer LIFO: outcomeCh closes LAST.
	go func() {
		defer close(outcomeCh)
		defer close(events)
		defer func() { _ = pr.Close() }()

		var capturedResult agent.AgentResult
		var kind string // "" none | "ok" | "unparseable" | "fatal"
		var captureErr error

		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			// Scanner reuses its buffer; copy the line since it outlives this
			// iteration as the event Payload.
			raw := append([]byte(nil), line...)
			ev, perr := parseStreamEvent(raw)
			if perr != nil {
				continue // tolerate a stray non-JSON line
			}
			select {
			case events <- agent.AgentEvent{Kind: ev.Type, Payload: raw, Stream: "stdout", Display: displayForDroid(ev)}:
			case <-ctx.Done():
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
				return
			}
			switch ev.Type {
			case "completion":
				res, eerr := resultFromCompletion(ev, inv)
				var unparseable *agent.ErrUnparseableOutput
				switch {
				case eerr == nil:
					capturedResult = res
					kind = "ok"
				case errors.As(eerr, &unparseable):
					kind = "unparseable"
				default:
					kind = "fatal"
					captureErr = eerr
				}
			case "error":
				kind = "fatal"
				captureErr = errorFromEvent(ev)
			}
		}
		if serr := scanner.Err(); serr != nil && kind == "" {
			kind = "fatal"
			captureErr = fmt.Errorf("agent/droid: scan stream-json: %w", serr)
		}

		execResult := <-resultCh
		stderrStr := <-stderrCh
		switch {
		case execResult.Err != nil:
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: execResult.Err}}
		case kind == "ok":
			outcomeCh <- agent.AgentOutcome{Result: capturedResult}
		case kind == "unparseable":
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrUnparseableOutput{NodePath: inv.NodePath}}
		case kind == "fatal" && errors.Is(captureErr, ErrAuthFailureSentinel):
			// Auth failure ("set a valid FACTORY_API_KEY") is a DETERMINISTIC fault —
			// classify PERMANENT (wrap agent.ErrPermissionDenied → permanent_failure)
			// so it fails fast instead of burning the 8-attempt retry budget,
			// consistent with the claude and awf/llm adapters. captureErr (wrapping
			// ErrAuthFailureSentinel) is kept in the chain.
			outcomeCh <- agent.AgentOutcome{Err: fmt.Errorf("agent/droid: authentication failed: %w: %w", agent.ErrPermissionDenied, captureErr)}
		case kind == "fatal":
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: captureErr}}
		default: // no terminal completion/error event seen
			if reason, ok := configErrorFromStderr(stderrStr); ok {
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrInvalidConfig{Ref: AdapterRef, Reason: reason}}
			} else {
				outcomeCh <- agent.AgentOutcome{Err: &ErrUnexpectedExit{ExitCode: execResult.ExitCode, Stderr: stderrStr}}
			}
		}
	}()

	return events, outcomeCh, nil
}

// configErrorPatterns are stable substrings droid prints to STDERR for
// deterministic config-validation failures (verified v0.138.0). Permanent —
// retrying an identical exec never fixes a bad model id or reasoning-effort.
var configErrorPatterns = []string{"Invalid model:", "Unsupported reasoning effort", "Invalid enum value", "Invalid --"}

func configErrorFromStderr(stderr string) (string, bool) {
	for _, p := range configErrorPatterns {
		if strings.Contains(stderr, p) {
			return firstNonEmptyLine([]byte(stderr)), true
		}
	}
	return "", false
}

// schemaDirective instructs droid to make a single JSON object its ENTIRE final
// message (the layer-2 path; extractJSONObject is tolerant of prose/fences as a
// backstop, but a clean directive minimizes parse failures → fewer gate repairs).
const schemaDirective = "\n\nIMPORTANT: when the task is complete, your FINAL message must be ONLY a single JSON object conforming exactly to this JSON Schema — no prose before or after, no code fences:\n"

// autonomyFlags maps the `autonomy` with-key to the droid exec flag args it
// produces, and is the single source of truth for the accepted set: ValidateConfig
// accepts a value iff it is a key here, and assembleCommand appends the value.
// "read-only" is droid's default mode and emits no flag.
var autonomyFlags = map[string][]string{
	"read-only": nil,
	"low":       {"--auto", "low"},
	"medium":    {"--auto", "medium"},
	"high":      {"--auto", "high"},
	"skip":      {"--skip-permissions-unsafe"},
}

func assembleCommand(inv agent.AgentInvocation) (string, error) {
	prompt, ok := inv.With[keyPrompt].(string)
	if !ok {
		return "", fmt.Errorf("agent/droid: assembleCommand: with.prompt missing or non-string")
	}
	prompt, err := agent.PrependFeedback(prompt, inv.Feedback)
	if err != nil {
		return "", fmt.Errorf("agent/droid: prepend gate feedback: %w", err)
	}
	if inv.OutputSchema != nil {
		schemaBytes, serr := json.Marshal(*inv.OutputSchema)
		if serr != nil {
			return "", fmt.Errorf("agent/droid: marshal OutputSchema: %w", serr)
		}
		prompt = prompt + schemaDirective + string(schemaBytes)
	}

	// BYOK (custom OpenAI-compatible endpoint): base_url set ⇒ write a one-entry
	// customModels settings file to the container (printf prelude, mirroring codex)
	// and reference the entry as `custom:<model>`. The prelude is PREPENDED with &&
	// so a printf failure short-circuits (droid never runs → no terminal event →
	// retryable, identical to codex). validateBYOK guarantees model+api_key_env are
	// non-empty strings here.
	var prelude string
	byok, _ := inv.With[keyBaseURL].(string)
	if byok != "" {
		settingsJSON, serr := byokSettingsJSON(inv)
		if serr != nil {
			return "", serr
		}
		prelude = "printf '%s' " + shellQuote(settingsJSON) + " > " + droidSettingsPath + " && "
	}

	parts := []string{"droid", "exec", "-o", "stream-json"}
	if byok != "" {
		parts = append(parts, "--settings", droidSettingsPath)
	}
	if model, ok := inv.With[keyModel].(string); ok && model != "" {
		ref := model
		if byok != "" {
			ref = "custom:" + model // the custom: prefix resolves the customModels entry
		}
		parts = append(parts, "--model", shellQuote(ref))
	}
	if re, ok := inv.With[keyEffort].(string); ok && re != "" {
		parts = append(parts, "--reasoning-effort", re) // value validated against a fixed enum
	}
	autonomy := "skip" // default: --skip-permissions-unsafe (isolated container)
	if v, ok := inv.With[keyAutonomy].(string); ok && v != "" {
		autonomy = v
	}
	parts = append(parts, autonomyFlags[autonomy]...) // read-only → nil → no-op
	if sp, ok := inv.With[keySystemPrompt].(string); ok && sp != "" {
		parts = append(parts, "--append-system-prompt", shellQuote(sp))
	}
	if tools := toStringSlice(inv.With[keyAllowedTools]); len(tools) > 0 {
		parts = append(parts, "--enabled-tools", shellQuote(strings.Join(tools, ",")))
	}
	if tools := toStringSlice(inv.With[keyDisallowedTools]); len(tools) > 0 {
		parts = append(parts, "--disabled-tools", shellQuote(strings.Join(tools, ",")))
	}
	parts = append(parts, shellQuote(prompt)) // positional prompt LAST
	return prelude + strings.Join(parts, " "), nil
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// shellQuote single-quotes s for `sh -c` consumption (POSIX-portable).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Compile-time assertion that Adapter satisfies agent.Adapter (all five methods
// now exist). This is the single canonical conformance assertion for the package.
var _ agent.Adapter = (*Adapter)(nil)
