package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
)

const (
	codexHomeDirName = "codex-home"
	codexBaseHomeEnv = "AWF_CODEX_BASE_HOME"
)

// invocationDigest is a deterministic, opaque identity for one logical adapter
// invocation. Attempt is deliberately excluded: retries of the same run/node/key
// reuse their isolated state, while another run, node, or idempotency identity
// gets a disjoint path. Hashing also keeps author-controlled identifiers out of
// filesystem paths and shell command strings.
func invocationDigest(inv agent.AgentInvocation) [sha256.Size]byte {
	return sha256.Sum256([]byte(inv.RunContext.RunID + "\x00" + inv.NodePath + "\x00" + inv.IdempotencyKey))
}

// codexHomePath returns the absolute, per-invocation CODEX_HOME below the
// backend's writable staging area. Docker/fake staging roots are already
// absolute in-container paths. Native's root is workdir-relative, so its
// WorkdirResolver maps the path below the handle's absolute workdir.
func codexHomePath(backend container.Backend, handle container.Handle, inv agent.AgentInvocation) (string, error) {
	root := backend.Capabilities().StagingRoot
	if root == "" {
		return "", errors.New("agent/codex: CODEX_HOME: backend has empty StagingRoot")
	}
	if !filepath.IsAbs(root) {
		resolver, ok := backend.(container.WorkdirResolver)
		if !ok {
			return "", fmt.Errorf("agent/codex: CODEX_HOME: relative StagingRoot %q requires container.WorkdirResolver", root)
		}
		root = resolver.ResolveWorkdirPath(handle, root)
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("agent/codex: CODEX_HOME: resolved StagingRoot %q is not absolute", root)
	}
	sum := invocationDigest(inv)
	return filepath.Join(filepath.Clean(root), codexHomeDirName, fmt.Sprintf("%x", sum)), nil
}

// codexSchemaPath returns the in-container path the invocation's output_schema is
// written to (via printf in the same sh -c command) for --output-schema. The hex
// digest is deterministic and shell-safe, hashes stable invocation identity,
// and isolates concurrent native invocations that share the host /tmp.
// Retries of the same node intentionally reuse (and truncate) the same path.
func codexSchemaPath(inv agent.AgentInvocation) string {
	sum := invocationDigest(inv)
	return fmt.Sprintf("/tmp/awf-codex-schema-%x.json", sum)
}

// Launch runs `codex exec --json ...` inside handle via the streaming Backend.Exec.
// codex's --json emits JSONL flushed live; Launch scans it and emits ONE AgentEvent
// per line, records the LAST agent_message text (last-wins), captures the terminal
// usage + any error/turn.failed message, then sends exactly one AgentOutcome.
//
// STREAMING GRANULARITY (accepted limitation): codex exec --json is EVENT-granular,
// not token-granular — each agent_message arrives as ONE complete item.completed
// line (no item.updated/text deltas). So events (tool calls, reasoning) render live,
// but the answer TEXT does NOT stream character-by-character like goose/claude.
// Token deltas exist only on codex's mcp-server/app-server JSON-RPC interface, which
// the stdin-less container.Cmd seam cannot drive. See the spec §1 streaming note.
//
// γ contract: returns IMMEDIATELY with both channels open; events closes BEFORE
// outcome (defer LIFO); never reuses a session.
func (a *Adapter) Launch(ctx context.Context, handle container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	if a.backend == nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: errors.New("agent/codex: Launch: no Backend wired (use WithBackend in New)")}
	}
	codexHome, err := codexHomePath(a.backend, handle, inv)
	if err != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}
	cmdString, err := assembleCommand(inv, a.env["OPENAI_API_KEY"] != "")
	if err != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}
	env := make(map[string]string, len(a.env)+3)
	for k, v := range a.env {
		env[k] = v
	}
	// Preserve the configured home only as the copy source for seed files, then
	// override CODEX_HOME before either login or exec can observe the process
	// environment. An explicit empty helper masks any inherited host value; the
	// shell prelude then uses its $HOME/.codex fallback.
	env[codexBaseHomeEnv] = a.env["CODEX_HOME"]
	env["CODEX_HOME"] = codexHome
	if inv.IdempotencyKey != "" {
		env["AWF_IDEMPOTENCY_KEY"] = inv.IdempotencyKey
	}

	chunks, resultCh, execErr := a.backend.Exec(ctx, handle, container.Cmd{Run: cmdString, Env: env, AgentRuntime: true})
	if execErr != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: execErr}
	}

	events := make(chan agent.AgentEvent, 16)
	outcomeCh := make(chan agent.AgentOutcome, 1)
	pr, pw := io.Pipe()
	stderrCh := make(chan string, 1)

	// Goroutine A: forward stdout chunks into the pipe (for line scanning) and
	// ACCUMULATE stderr. codex's --json is stdout-only, but stderr carries the
	// cosmetic "Reading additional input from stdin..." notice AND real diagnostics
	// on an early/transport failure — capture it as the fallback diagnostic
	// (Goroutine B uses it only when stdout yielded no usable result). Hands the
	// stderr text off on the buffered channel after closing the pipe writer.
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
	// (event-granular live progress; the answer TEXT is one atomic agent_message line),
	// record last-wins agent_message + terminal usage + turn-failure/error messages
	// + diag, then send exactly one AgentOutcome. defer LIFO: outcomeCh closes LAST.
	go func() {
		defer close(outcomeCh)
		defer close(events)
		defer func() { _ = pr.Close() }()

		var finalText string
		var haveFinal bool
		var usage *usageRec
		var sawTurnCompleted bool
		var sawTurnFailed bool  // terminal turn.failed seen
		var turnFailMsg string  // turn.failed error.message
		var lastErrorMsg string // last bare `error` event (transient OR terminal-without-turn.failed)
		var diag bytes.Buffer

		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			raw := append([]byte(nil), line...) // Scanner reuses its buffer; the line outlives this iteration as Payload.
			ev, perr := parseStreamEvent(raw)
			if perr != nil {
				diag.Write(raw)
				diag.WriteByte('\n')
				continue
			}
			select {
			case events <- agent.AgentEvent{Kind: eventKind(ev), Payload: raw, Stream: "stdout", Display: displayForCodex(ev)}:
			case <-ctx.Done():
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
				return
			}
			if txt, ok := agentMessageText(ev); ok {
				finalText = txt
				haveFinal = true
			}
			switch ev.Type {
			case "turn.completed":
				sawTurnCompleted = true
				usage = ev.Usage
			case "turn.failed":
				sawTurnFailed = true
				if ev.Error != nil {
					turnFailMsg = ev.Error.Message
				}
			case "error":
				// A bare `error` event can be a NON-FATAL transient notice (codex emits
				// e.g. "Reconnecting... 1/5" while retrying a dropped stream, then
				// continues to turn.completed), OR a terminal error with no turn.failed.
				// Capture it as a fallback only — the failure verdict is driven by
				// turn.failed below, so a transient error followed by turn.completed
				// still succeeds (engine invariant: crash ≠ verdict).
				lastErrorMsg = ev.Message
			}
		}
		if serr := scanner.Err(); serr != nil && !sawTurnFailed {
			fmt.Fprintf(&diag, "scan codex --json: %v\n", serr)
		}

		execResult := <-resultCh
		stderrStr := <-stderrCh // drain goroutine A (also the fallback diagnostic)
		// Prefer stdout-diag; fall back to stderr only when stdout yielded nothing
		// (codex's cosmetic stdin notice lands on stderr, so don't surface it unless
		// it's the only signal we have). Mirrors agent/goose/launch.go's guard.
		diagLine := firstNonEmptyLine(diag.Bytes())
		if diagLine == "" {
			diagLine = firstNonEmptyLine([]byte(stderrStr))
		}

		switch {
		case execResult.Err != nil:
			// Transport-class error (backend died mid-stream): retryable.
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: execResult.Err}}

		case sawTurnCompleted && haveFinal && strings.TrimSpace(finalText) != "":
			// SUCCESS — terminal turn.completed + a non-empty agent_message. Evaluated
			// BEFORE the failure branches so a non-fatal `error` event (a transient
			// reconnect notice) the turn recovered from never masks a good result.
			// (turn.completed and turn.failed are mutually exclusive, so this can't
			// race the sawTurnFailed branch.)
			res, eerr := buildResult(finalText, usage, inv, a.pricer)
			var unparseable *agent.ErrUnparseableOutput
			switch {
			case eerr == nil:
				outcomeCh <- agent.AgentOutcome{Result: res}
			case errors.As(eerr, &unparseable):
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrUnparseableOutput{NodePath: inv.NodePath}}
			default:
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: eerr}}
			}

		case sawTurnFailed:
			// Terminal failure. Prefer the turn.failed message; fall back to the
			// preceding `error` event text (codex's wire pattern is error→turn.failed).
			msg := turnFailMsg
			if msg == "" {
				msg = lastErrorMsg
			}
			switch {
			case msg == "":
				// turn.failed with no message AND no preceding error event: surface the
				// exit code + any stdout/stderr diagnostic instead of a content-free
				// "codex turn failed:". Retryable (no permanent signal present).
				outcomeCh <- agent.AgentOutcome{Err: &ErrUnexpectedExit{ExitCode: execResult.ExitCode, Output: diagLine}}
			case isPermanentCodexError(msg):
				// HTTP 400 + invalid_request_error (bad model / schema codex rejects): permanent.
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrInvalidConfig{Ref: AdapterRef, Reason: firstNonEmptyLine([]byte(msg))}}
			default:
				// auth, rate-limit, 5xx, provider/transport fault: retryable. (A
				// codex-LOCAL config-load rejection — e.g. a bad provisioned config.toml —
				// also lands retryable here or in the default branch, not permanent: the
				// adapter enum-validates the knobs it sets, and a bad output_schema
				// surfaces as an API 400 above; the residual edge is bounded by the gate
				// attempt budget.)
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: fmt.Errorf("agent/codex: codex turn failed: %s", msg)}}
			}

		case sawTurnCompleted:
			// turn.completed but no agent_message text: retryable.
			outcomeCh <- agent.AgentOutcome{Err: &ErrUnexpectedExit{ExitCode: execResult.ExitCode, Output: "codex produced no agent message"}}

		case lastErrorMsg != "":
			// A terminal `error` event with NO turn.failed/turn.completed (codex's
			// "unrecoverable error emitted directly by the event stream").
			if isPermanentCodexError(lastErrorMsg) {
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrInvalidConfig{Ref: AdapterRef, Reason: firstNonEmptyLine([]byte(lastErrorMsg))}}
			} else {
				outcomeCh <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: fmt.Errorf("agent/codex: codex error: %s", lastErrorMsg)}}
			}

		default:
			// No terminal event at all (codex died before completing): retryable.
			outcomeCh <- agent.AgentOutcome{Err: &ErrUnexpectedExit{ExitCode: execResult.ExitCode, Output: diagLine}}
		}
	}()

	return events, outcomeCh, nil
}

// assembleCommand builds the full `codex exec ...` shell-command string. Every
// command first creates the already-env-selected isolated CODEX_HOME with a 077
// umask and seeds missing auth.json/config.toml files from AWF_CODEX_BASE_HOME
// (or $HOME/.codex). cp moves bytes directly between files: credential/config
// values never enter the shell command, argv, or output. Existing files win so a
// deterministic retry can retain Codex's refreshed auth and other mutable state.
// The base-home helper is unset before login/exec so Codex and its child tools
// see only the isolated CODEX_HOME.
//
// When OutputSchema is set, a `printf '%s' <json> > FILE &&` prelude writes the
// schema to the container before codex reads it via --output-schema (both
// backends run cmd.Run through sh -c). Feedback is prepended to the prompt; the
// prompt rides as the last positional after `--`, with stdin from /dev/null
// (silences codex's stdin notice; the appended <stdin> block is then empty).
// User strings are POSIX-single-quoted via shellQuote. The prompt+feedback ride
// as one sh -c argv element (128 KiB MAX_ARG_STRLEN ceiling, inherited from the
// sibling adapters); the schema-to-file write keeps the --output-schema value
// tiny (a path).
//
// apiKeyAuth (adapter env carries OPENAI_API_KEY): after seeding, run an
// idempotent login when the isolated home still has no auth.json. codex ≥ some
// 0.14x does NOT honor a bare OPENAI_API_KEY env var for exec auth — it needs
// auth.json materialized (`codex login --with-api-key` reading stdin; proven
// against 0.146.0 on 2026-08-16: env-only → 401 "Missing bearer", login first →
// green). printenv keeps the key out of argv; login chatter goes to stderr so
// stdout stays clean --json.
func assembleCommand(inv agent.AgentInvocation, apiKeyAuth bool) (string, error) {
	prompt, ok := inv.With[keyPrompt].(string)
	if !ok {
		return "", fmt.Errorf("agent/codex: assembleCommand: with.prompt missing or non-string")
	}
	prompt, err := agent.PrependFeedback(prompt, inv.Feedback)
	if err != nil {
		return "", fmt.Errorf("agent/codex: prepend gate feedback: %w", err)
	}

	// CODEX_HOME is set by Launch to an absolute, opaque per-invocation path.
	// Seed only missing regular destinations and refuse to follow a pre-existing
	// destination symlink. The restrictive umask protects the copy immediately;
	// chmod pins the intended mode independently of source permissions.
	prelude := `umask 077; mkdir -p "$CODEX_HOME" && awf_codex_base="${AWF_CODEX_BASE_HOME:-$HOME/.codex}" && if [ "$awf_codex_base" != "$CODEX_HOME" ]; then for awf_codex_file in auth.json config.toml; do if [ -f "$awf_codex_base/$awf_codex_file" ] && [ ! -e "$CODEX_HOME/$awf_codex_file" ] && [ ! -L "$CODEX_HOME/$awf_codex_file" ]; then cp "$awf_codex_base/$awf_codex_file" "$CODEX_HOME/$awf_codex_file" || exit 1; chmod 600 "$CODEX_HOME/$awf_codex_file" || exit 1; fi; done; fi && unset AWF_CODEX_BASE_HOME awf_codex_base awf_codex_file && `
	if apiKeyAuth {
		prelude += `{ [ -f "$CODEX_HOME/auth.json" ] || { printenv OPENAI_API_KEY | codex login --with-api-key >&2; }; } && `
	}
	parts := []string{"codex", "exec", "--json", "--skip-git-repo-check", "--ephemeral"}

	if inv.OutputSchema != nil {
		schemaBytes, serr := json.Marshal(*inv.OutputSchema)
		if serr != nil {
			return "", fmt.Errorf("agent/codex: marshal OutputSchema: %w", serr)
		}
		schemaPath := codexSchemaPath(inv)
		prelude += "printf '%s' " + shellQuote(string(schemaBytes)) + " > " + schemaPath + " && "
		parts = append(parts, "--output-schema", schemaPath)
	}

	// Sandbox: an explicit non-empty key uses codex's internal sandbox; otherwise the
	// AWF container is the boundary → bypass codex's approvals+sandbox entirely.
	if sandbox, ok := inv.With[keySandbox].(string); ok && sandbox != "" {
		parts = append(parts, "--sandbox", shellQuote(sandbox))
	} else {
		parts = append(parts, "--dangerously-bypass-approvals-and-sandbox")
	}
	if model, ok := inv.With[keyModel].(string); ok && model != "" {
		parts = append(parts, "-m", shellQuote(model))
	}
	if re, ok := inv.With[keyEffort].(string); ok && re != "" {
		parts = append(parts, "-c", shellQuote("model_reasoning_effort="+re))
	}
	parts = append(parts, "--", shellQuote(prompt))

	// "</dev/null" is a SHELL REDIRECT appended AFTER the Join (NOT an argv element —
	// never shellQuote it; it must stay last). It is load-bearing: without it
	// `codex exec` can hang INDEFINITELY waiting for stdin EOF on a non-TTY pipe
	// (codex#20919); it also empties the appended <stdin> block so codex's cosmetic
	// "Reading additional input from stdin..." notice stays harmless.
	return prelude + strings.Join(parts, " ") + " </dev/null", nil
}

// shellQuote single-quotes s for `sh -c` consumption (POSIX). COPIED verbatim from
// the sibling adapters (package-private; agent/ exports no shellQuote).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Compile-time assertion that Adapter satisfies agent.Adapter (all five methods).
var _ agent.Adapter = (*Adapter)(nil)
