package droid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
)

// Launch runs `droid exec -o json ...` inside handle. Because -o json emits a
// SINGLE result envelope (no streaming), one goroutine suffices: it drains the
// chunks channel into stdout/stderr buffers, reads the ExecResult, parses the
// envelope, emits ONE AgentEvent (the raw line), and sends ONE AgentOutcome.
// γ contract: returns IMMEDIATELY with both channels open; events closes BEFORE
// outcome (defer order); never reuses a session.
func (a *Adapter) Launch(ctx context.Context, handle container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	if a.backend == nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: errors.New("agent/droid: Launch: no Backend wired (use WithBackend in New)")}
	}
	cmdString, err := assembleCommand(inv)
	if err != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}

	// Exec env = forwarded allowlist + opsec hardening + idempotency.
	env := make(map[string]string, len(a.env)+3)
	for k, v := range a.env {
		env[k] = v
	}
	// Opsec (verified, droid v0.138.0): the CLI's OTel tracer is hardcoded-on and
	// exports to telemetry.factory.ai unless OTEL_SDK_DISABLED=true; the
	// customer-export path is gated by OTEL_CUSTOMER_ENABLED. We disable both so an
	// AWF run leaks no telemetry. (FACTORY_OTEL_ENABLED is NOT read by the CLI.)
	// cloudSessionSync (mirrors session content to Factory's web app, ON by
	// default) has no env knob — disable it operationally in the image; see Task 8.
	env["OTEL_SDK_DISABLED"] = "true"
	env["OTEL_CUSTOMER_ENABLED"] = "false"
	if inv.IdempotencyKey != "" {
		env["AWF_IDEMPOTENCY_KEY"] = inv.IdempotencyKey
	}

	chunks, resultCh, execErr := a.backend.Exec(ctx, handle, container.Cmd{Run: cmdString, Env: env})
	if execErr != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: execErr}
	}

	events := make(chan agent.AgentEvent, 1)
	outcomeCh := make(chan agent.AgentOutcome, 1)

	go func() {
		defer close(outcomeCh) // closes LAST
		defer close(events)    // closes BEFORE outcomeCh

		var stdout, stderr bytes.Buffer
		for c := range chunks { // drain fully (or the streaming backend can block)
			switch c.Stream {
			case "stdout":
				stdout.Write(c.Data)
			case "stderr":
				stderr.Write(c.Data)
			}
		}
		execResult := <-resultCh
		if execResult.Err != nil { // transport/mid-stream fault → retryable
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: execResult.Err}}
			return
		}

		rawLine, parsed := lastEnvelope(stdout.Bytes())
		if parsed == nil {
			// No result envelope. droid prints config-validation errors (invalid
			// model / bad reasoning-effort) to STDERR with empty stdout + exit 1 —
			// deterministic, PERMANENT. Everything else → retryable unexpected exit.
			if reason, ok := configErrorFromStderr(stderr.String()); ok {
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrInvalidConfig{Ref: AdapterRef, Reason: reason}}
				return
			}
			outcomeCh <- agent.AgentOutcome{Err: &ErrUnexpectedExit{ExitCode: execResult.ExitCode, Stderr: stderr.String()}}
			return
		}

		// One event carrying the raw envelope line (lossless transcript).
		select {
		case events <- agent.AgentEvent{Kind: "result", Payload: rawLine, Stream: "stdout"}:
		case <-ctx.Done():
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
			return
		}

		res, eerr := extractResult(parsed, inv)
		var unparseable *agent.ErrUnparseableOutput
		switch {
		case eerr == nil:
			outcomeCh <- agent.AgentOutcome{Result: res}
		case errors.As(eerr, &unparseable):
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrUnparseableOutput{NodePath: inv.NodePath}}
		default: // auth + other in-flight failures → retryable
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: eerr}}
		}
	}()

	return events, outcomeCh, nil
}

// lastEnvelope returns the raw bytes + parsed form of droid's result envelope.
// -o json emits exactly one JSON object; parse the whole trimmed buffer, else
// scan lines bottom-up for the last that parses (tolerates a stray stdout line).
// Returns (nil, nil) if nothing parses.
func lastEnvelope(stdout []byte) ([]byte, *execEnvelope) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if env, err := parseEnvelope(trimmed); err == nil && env.Type != "" {
		return trimmed, env
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		if env, err := parseEnvelope(line); err == nil && env.Type != "" {
			return line, env
		}
	}
	return nil, nil
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
	prompt, ok := inv.With["prompt"].(string)
	if !ok {
		return "", fmt.Errorf("agent/droid: assembleCommand: with.prompt missing or non-string")
	}
	if len(inv.Feedback) > 0 {
		fb, ferr := json.Marshal(inv.Feedback)
		if ferr != nil {
			return "", fmt.Errorf("agent/droid: marshal Feedback: %w", ferr)
		}
		prompt = fmt.Sprintf("<previous verdict>\n%s\n\n%s", string(fb), prompt)
	}
	if inv.OutputSchema != nil {
		schemaBytes, serr := json.Marshal(*inv.OutputSchema)
		if serr != nil {
			return "", fmt.Errorf("agent/droid: marshal OutputSchema: %w", serr)
		}
		prompt = prompt + schemaDirective + string(schemaBytes)
	}

	parts := []string{"droid", "exec", "-o", "json"}
	if model, ok := inv.With["model"].(string); ok && model != "" {
		parts = append(parts, "--model", shellQuote(model))
	}
	if re, ok := inv.With["reasoning_effort"].(string); ok && re != "" {
		parts = append(parts, "--reasoning-effort", re) // value validated against a fixed enum
	}
	autonomy := "skip" // default: --skip-permissions-unsafe (isolated container)
	if v, ok := inv.With["autonomy"].(string); ok && v != "" {
		autonomy = v
	}
	parts = append(parts, autonomyFlags[autonomy]...) // read-only → nil → no-op
	if sp, ok := inv.With["system_prompt"].(string); ok && sp != "" {
		parts = append(parts, "--append-system-prompt", shellQuote(sp))
	}
	if tools := toStringSlice(inv.With["enabled_tools"]); len(tools) > 0 {
		parts = append(parts, "--enabled-tools", shellQuote(strings.Join(tools, ",")))
	}
	if tools := toStringSlice(inv.With["disabled_tools"]); len(tools) > 0 {
		parts = append(parts, "--disabled-tools", shellQuote(strings.Join(tools, ",")))
	}
	parts = append(parts, shellQuote(prompt)) // positional prompt LAST
	return strings.Join(parts, " "), nil
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
