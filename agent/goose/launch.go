package goose

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
)

// gooseEphemeralState is the in-container path goose's data+state (cleartext
// transcript sqlite + logs) are redirected to via XDG_DATA_HOME/XDG_STATE_HOME, so
// they never land in the operator's home/workspace (goose persists them even under
// --no-session). A fixed path keeps Launch deterministic (no rand); it assumes no
// two goose execs share one container concurrently — true for AWF's sequential gate
// and per-container map model.
const gooseEphemeralState = "/tmp/awf-goose"

// configErrorPatterns — deterministic substrings goose prints to STDOUT (stderr is
// empty) with exit 1 for config-validation failures (verified v1.36.0). Permanent:
// retrying an identical exec never fixes a bad provider or a missing-key config.
var configErrorPatterns = []string{"error: Error Unknown provider:", "error: Error Configuration value not found:"}

func isConfigError(stdout string) bool {
	for _, p := range configErrorPatterns {
		if strings.Contains(stdout, p) {
			return true
		}
	}
	return false
}

// schemaDirective instructs goose to make a single JSON object its ENTIRE final
// message (layer-2). The worked example is a cosmetic nudge (layer-2 purity was
// clean with and without it); extractJSONObject is the tolerant backstop.
const schemaDirective = "\n\nIMPORTANT: when the task is complete, your FINAL message must be ONLY a single JSON object conforming exactly to this JSON Schema — no prose before or after, no code fences. For example, your entire final message would be exactly: {\"field\": value}\nSchema:\n"

// Launch runs `goose run -q --output-format stream-json --no-session ...` inside
// handle via the streaming Backend.Exec. goose's stream-json emits NDJSON flushed
// LIVE: "message" deltas (assistant text, incremental), then a terminal "complete"
// or "error". Launch scans those lines and emits ONE AgentEvent per line as it
// arrives (the realtime tap renders these live), accumulating assistant text deltas
// into finalText for the output layer-2 path.
// γ contract: returns IMMEDIATELY with both channels open; events closes BEFORE
// outcome (defer LIFO); never reuses a session.
func (a *Adapter) Launch(ctx context.Context, handle container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	if a.backend == nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: errors.New("agent/goose: Launch: no Backend wired (use WithBackend in New)")}
	}
	cmdString, err := assembleCommand(inv)
	if err != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}
	env := make(map[string]string, len(a.env)+6)
	for k, v := range a.env {
		env[k] = v
	}
	// Opsec (verified goose v1.36.0): force auto mode, disable keyring access,
	// disable telemetry, and redirect goose's sqlite transcript + logs to an
	// ephemeral in-container path (goose always writes state even under --no-session).
	env["GOOSE_MODE"] = "auto"
	env["GOOSE_DISABLE_KEYRING"] = "1"
	env["GOOSE_TELEMETRY_ENABLED"] = "false"
	env["XDG_DATA_HOME"] = gooseEphemeralState
	env["XDG_STATE_HOME"] = gooseEphemeralState
	if inv.IdempotencyKey != "" {
		env["AWF_IDEMPOTENCY_KEY"] = inv.IdempotencyKey
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
	// accumulate stderr (for the transport-error path). Hands the stderr off on a
	// buffered channel after closing the pipe writer.
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
	// (this is the realtime path), accumulate assistant-text deltas into finalText,
	// capture terminal complete/error, then send exactly one AgentOutcome.
	// defer LIFO: outcomeCh closes LAST (γ contract: events closes before outcome).
	go func() {
		defer close(outcomeCh)
		defer close(events)
		defer func() { _ = pr.Close() }()

		var finalText strings.Builder
		var nonJSON strings.Builder
		var errorEventText string
		var sawComplete bool
		var completeEv *streamEvent

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
				// goose prints non-JSON diagnostics to stdout before exit 1
				// (e.g. "  error: Error Unknown provider: nope."). Capture them
				// for the config-error classifier rather than discarding silently.
				nonJSON.Write(raw)
				nonJSON.WriteByte('\n')
				continue
			}
			select {
			case events <- agent.AgentEvent{Kind: ev.Type, Payload: raw, Stream: "stdout", Display: displayForGoose(ev)}:
			case <-ctx.Done():
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
				return
			}
			switch ev.Type {
			case "message":
				finalText.WriteString(assistantText(ev))
			case "complete":
				sawComplete = true
				completeEv = ev
			case "error":
				errorEventText = ev.Error
			}
		}
		if serr := scanner.Err(); serr != nil && errorEventText == "" {
			errorEventText = fmt.Sprintf("scan stream-json: %v", serr)
		}

		execResult := <-resultCh
		stderrStr := <-stderrCh
		ft := finalText.String()
		// diag: stdout non-JSON lines are goose's primary error channel; fall
		// back to stderr if stdout was empty (should not happen under goose, but
		// defensive).
		diag := nonJSON.String()
		if strings.TrimSpace(diag) == "" && strings.TrimSpace(stderrStr) != "" {
			diag = stderrStr
		}

		switch {
		case execResult.Err != nil:
			// Transport-class error (backend died mid-stream): retryable.
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: execResult.Err}}

		case execResult.ExitCode != 0 && isConfigError(diag):
			// Config-validation failure (goose stdout "error: Error Unknown provider: ..."
			// + exit 1): permanent — retrying an identical exec never fixes a bad provider.
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrInvalidConfig{Ref: AdapterRef, Reason: firstNonEmptyLine([]byte(diag))}}

		case errorEventText != "" || (execResult.ExitCode != 0 && strings.TrimSpace(diag) != ""):
			// Stream-level error event (provider fault, auth failure) or a nonzero
			// exit with diagnostic output not matching config patterns: retryable.
			msg := errorEventText
			if msg == "" {
				msg = firstNonEmptyLine([]byte(diag))
			}
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: fmt.Errorf("agent/goose: goose run error: %s", msg)}}

		case sawComplete && strings.TrimSpace(ft) != "":
			// Happy path: terminal complete event and non-empty assistant text.
			res, eerr := buildResult(ft, completeEv, inv)
			var unparseable *agent.ErrUnparseableOutput
			switch {
			case eerr == nil:
				outcomeCh <- agent.AgentOutcome{Result: res}
			case errors.As(eerr, &unparseable):
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrUnparseableOutput{NodePath: inv.NodePath}}
			default:
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: eerr}}
			}

		case sawComplete:
			// complete event but zero assistant text — bad model / unknown-model trap.
			// Retryable: the engine may retry with a different model config.
			outcomeCh <- agent.AgentOutcome{Err: &ErrUnexpectedExit{ExitCode: execResult.ExitCode, Output: "goose produced no output (possible unknown model)"}}

		default:
			// No terminal complete/error event at all.
			outcomeCh <- agent.AgentOutcome{Err: &ErrUnexpectedExit{ExitCode: execResult.ExitCode, Output: firstNonEmptyLine([]byte(diag))}}
		}
	}()

	return events, outcomeCh, nil
}

// assembleCommand builds the `goose run ...` shell command string for inv.
// Feedback is prepended to the prompt; OutputSchema appends the schema directive.
// The prompt is single-quoted last (POSIX-safe via shellQuote).
func assembleCommand(inv agent.AgentInvocation) (string, error) {
	prompt, ok := inv.With[keyPrompt].(string)
	if !ok {
		return "", fmt.Errorf("agent/goose: assembleCommand: with.prompt missing or non-string")
	}
	if len(inv.Feedback) > 0 {
		fb, ferr := json.Marshal(inv.Feedback)
		if ferr != nil {
			return "", fmt.Errorf("agent/goose: marshal Feedback: %w", ferr)
		}
		prompt = fmt.Sprintf("<previous verdict>\n%s\n\n%s", string(fb), prompt)
	}
	if inv.OutputSchema != nil {
		schemaBytes, serr := json.Marshal(*inv.OutputSchema)
		if serr != nil {
			return "", fmt.Errorf("agent/goose: marshal OutputSchema: %w", serr)
		}
		prompt = prompt + schemaDirective + string(schemaBytes)
	}

	parts := []string{"goose", "run", "-q", "--output-format", "stream-json", "--no-session"}
	if model, ok := inv.With[keyModel].(string); ok && model != "" {
		parts = append(parts, "--model", shellQuote(model))
	}
	if mt, ok := inv.With[keyMaxTurns]; ok {
		if n, ok := asInt(mt); ok {
			parts = append(parts, "--max-turns", strconv.Itoa(n))
		}
	}
	if sp, ok := inv.With[keySystemPrompt].(string); ok && sp != "" {
		parts = append(parts, "--system", shellQuote(sp))
	}
	parts = append(parts, "-t", shellQuote(prompt))
	return strings.Join(parts, " "), nil
}

// shellQuote single-quotes s for `sh -c` consumption (POSIX). Copied from agent/droid.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Compile-time assertion that Adapter satisfies agent.Adapter (all five methods
// now exist). This is the single canonical conformance assertion for the package.
var _ agent.Adapter = (*Adapter)(nil)
