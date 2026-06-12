package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
)

// Launch runs `claude -p ...` inside handle via the streaming Backend.Exec.
// Returns IMMEDIATELY under the γ contract: events channel and outcome
// channel are both OPEN; the parser goroutine writes each AgentEvent as
// it's parsed from claude's stream-json stdout, then sends AgentOutcome
// on outcomeCh when the result event arrives + chunks close. The caller
// drains events concurrently — that's how `[<kind>] …` lines render
// progressively in stderr (realtime UX requirement).
//
// Phase 5 design decision 7: NO --continue / --resume / --session-id;
// ALWAYS --no-session-persistence.
func (a *Adapter) Launch(ctx context.Context, handle container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	if a.backend == nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: errors.New("agent/claude: Launch: no Backend wired (use WithBackend in New)")}
	}

	cmdString, err := assembleCommand(inv)
	if err != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}

	execCmd := container.Cmd{
		Run: cmdString,
		Env: map[string]string(a.env), // adapter-captured env allowlist
	}
	if inv.IdempotencyKey != "" {
		// Mirror runCode's idempotency-key injection (spec §10).
		if execCmd.Env == nil {
			execCmd.Env = map[string]string{}
		}
		execCmd.Env["AWF_IDEMPOTENCY_KEY"] = inv.IdempotencyKey
	}

	chunks, resultCh, execErr := a.backend.Exec(ctx, handle, execCmd)
	if execErr != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: execErr}
	}

	// γ contract: both channels returned OPEN.
	events := make(chan agent.AgentEvent, 16)
	outcomeCh := make(chan agent.AgentOutcome, 1)

	// io.Pipe bridges chunks → io.Reader so bufio.Scanner handles line
	// splitting. Same pattern as container/docker/snapshot.go.
	pr, pw := io.Pipe()

	// Goroutine A: forward stdout chunks into the pipe writer.
	// stderr chunks are dropped — claude's stream-json output is stdout-only.
	go func() {
		defer func() { _ = pw.Close() }()
		for c := range chunks {
			if c.Stream != "stdout" {
				continue
			}
			if _, werr := pw.Write(c.Data); werr != nil {
				// Pipe reader closed early — fall through to drain remaining
				// chunks so the streaming Backend can finish cleanly without
				// blocking on a stuck channel.
				for range chunks {
				}
				return
			}
		}
	}()

	// Goroutine B: scan lines from the pipe, EMIT EVENTS PROGRESSIVELY into
	// the events channel (THIS IS THE REALTIME PATH — each event reaches the
	// caller's range loop as the line is parsed), capture the result event,
	// then send AgentOutcome on outcomeCh when the stream ends.
	//
	// defer LIFO: declared first runs last. outcomeCh closes LAST (after
	// events closes) so the documented γ contract holds.
	go func() {
		defer close(outcomeCh)
		defer close(events)
		defer func() { _ = pr.Close() }()

		var capturedResult agent.AgentResult
		// kind: "" = no result event yet, "ok" = success,
		// "unparseable" = error_max_structured_output_retries,
		// "auth" = result.is_error:true (auth failure),
		// "fatal" = other extract error.
		var kind string
		var captureErr error
		var initModel string // model from system/init for Metrics.Model

		scanner := bufio.NewScanner(pr)
		// Default Scanner buffer is 64KiB; stream-json system/init lines run
		// ~5KiB but assistant messages with embedded tool_result can be
		// larger. 1MiB ceiling matches what state.Log allows inline.
		scanner.Buffer(make([]byte, 64*1024), 1<<20)

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			msg, perr := parseStreamLine(line)
			if perr != nil {
				captureErr = perr
				continue
			}
			// Capture the model reported in system/init for auditability.
			if msg.Type == "system" && msg.Subtype == "init" && msg.Model != "" {
				initModel = msg.Model
			}
			// PROGRESSIVE EMISSION: events fan out to the caller HERE,
			// AS SOON AS each stream-json line parses. ctx-aware send
			// so the caller dropping the channel doesn't hang us
			// indefinitely.
			for _, ev := range messageToEvents(msg) {
				select {
				case events <- ev:
				case <-ctx.Done():
					outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
					return
				}
			}
			if msg.Type == "result" {
				res, eerr := extractResult(msg, initModel)
				switch {
				case eerr == nil:
					capturedResult = res
					kind = "ok"
				case errors.Is(eerr, ErrAuthFailureSentinel):
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
			captureErr = fmt.Errorf("agent/claude: scan stream-json: %w", serr)
		}

		// Await the streaming Backend's ExecResult (chunks already closed).
		execResult := <-resultCh

		switch {
		case execResult.Err != nil:
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: execResult.Err}}
		case kind == "ok":
			outcomeCh <- agent.AgentOutcome{Result: capturedResult}
		case kind == "unparseable":
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrUnparseableOutput{NodePath: inv.NodePath}}
		case kind == "auth", kind == "fatal":
			// Both auth-failure (subtype:success + is_error:true) and other
			// extract errors surface as ErrAgentLaunch wrapping captureErr.
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: captureErr}}
		default: // kind == "" — no result event was seen
			outcomeCh <- agent.AgentOutcome{Err: &ErrUnexpectedExit{ExitCode: execResult.ExitCode, Stderr: ""}}
		}
	}()

	return events, outcomeCh, nil
}

// assembleCommand builds the full `claude -p ... ` shell-command string.
// Shell-escapes user-controlled strings via POSIX single-quoting (see
// shellQuote below for the literal escape sequence).
func assembleCommand(inv agent.AgentInvocation) (string, error) {
	prompt, ok := inv.With[keyPrompt].(string)
	if !ok {
		return "", fmt.Errorf("agent/claude: assembleCommand: with.prompt missing or non-string")
	}
	if len(inv.Feedback) > 0 {
		fb, ferr := json.Marshal(inv.Feedback)
		if ferr != nil {
			return "", fmt.Errorf("agent/claude: marshal Feedback: %w", ferr)
		}
		prompt = fmt.Sprintf("<previous verdict>\n%s\n\n%s", string(fb), prompt)
	}

	var parts []string
	parts = append(parts, "claude", "-p", shellQuote(prompt))
	// --verbose is REQUIRED when --print AND --output-format stream-json
	// are both set. Without it, claude exits with "Error: When using --print,
	// --output-format=stream-json requires --verbose".
	parts = append(parts, "--output-format", "stream-json", "--verbose")
	parts = append(parts, "--no-session-persistence")

	if inv.OutputSchema != nil {
		schemaBytes, serr := json.Marshal(*inv.OutputSchema)
		if serr != nil {
			return "", fmt.Errorf("agent/claude: marshal OutputSchema: %w", serr)
		}
		parts = append(parts, "--json-schema", shellQuote(string(schemaBytes)))
	}

	// bare default true per decision 9.
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

// shellQuote single-quotes s for `sh -c` consumption. Embedded single
// quotes are escaped using the POSIX-portable pattern: close-quote,
// backslash-quote, reopen-quote (see the literal in the body).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// toInt coerces an `any` decoded from YAML (which may be int, int64, or
// float64) into an int. Returns (0, false) if the value can't be coerced.
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

// Compile-time assertion that Adapter satisfies agent.Adapter (all methods
// now defined: Ref, Capabilities, Version, ValidateConfig, Launch).
var _ agent.Adapter = (*Adapter)(nil)
