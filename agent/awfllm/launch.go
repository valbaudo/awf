package awfllm

import (
	"context"
	"errors"
	"fmt"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
)

const eventsBuffer = 16

// Launch honors the γ contract: returns immediately with both channels open. A
// single goroutine drives the stream, emitting one AgentEvent per delta
// (DisplayAssistantDelta — renders char-by-char), then a DisplayFinal, then the
// outcome. The container.Handle is ignored (this adapter calls HTTP directly).
func (a *Adapter) Launch(ctx context.Context, _ container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	cfg, err := a.buildReqConfig(inv)
	if err != nil {
		return nil, nil, err // pre-launch failure: both channels nil
	}
	var total int
	for _, f := range inv.InputFiles {
		total += len(f.Content)
	}
	if cfg.MaxInlineBytes > 0 && total > cfg.MaxInlineBytes {
		return nil, nil, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "input_files", Reason: fmt.Sprintf("inline file bytes %d exceed max_inline_bytes %d; use a smaller document or the provider File API (out of scope)", total, cfg.MaxInlineBytes)}
	}
	if cfg.CacheContext && len(inv.ContextEvidence) == 0 {
		return nil, nil, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: keyCacheContext, Reason: "requires evaluator context evidence"}
	}
	prompt := assemblePrompt(inv)

	events := make(chan agent.AgentEvent, eventsBuffer)
	outcomeCh := make(chan agent.AgentOutcome, 1)

	go func() {
		// LIFO: events closes before outcomeCh (documented order).
		defer close(outcomeCh)
		defer close(events)

		var deltaSanitizer agent.DisplayStreamSanitizer
		emit := func(delta string, _ []byte) {
			displayText := agent.RedactDisplayText(deltaSanitizer.SanitizeText(delta))
			ev := agent.AgentEvent{
				Kind:    "delta",
				Stream:  "stdout",
				Live:    true,
				Payload: []byte(displayText),
				// Sanitized/redacted delta text — NOT agent.Elide (Elide clips a
				// >512B line with an ellipsis marker, which would corrupt the live
				// stream). DisplayAssistantDelta makes the renderer write it
				// WITHOUT a trailing newline, so deltas concatenate character-by-character.
				Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: displayText},
			}
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		}

		full, usage, wireModel, finish, serr := a.stream(ctx, cfg, prompt, inv.OutputSchema, inv.Thread, inv.ContextEvidence, inv.InputFiles, emit)
		if serr != nil {
			errText := liveEventText(serr.Error())
			// spec §B.7 step 4: emit a terminal DisplayError event before the
			// outcome so the live renderer can terminate the in-progress delta
			// line and display the error prominently. The ctx-aware send mirrors
			// the emit helper above — if the context is already done we skip the
			// event but still send the outcome below.
			select {
			case events <- agent.AgentEvent{Kind: "error", Stream: "stderr", Live: true, Payload: []byte(errText), Display: agent.EventDisplay{Class: agent.DisplayError, IsError: true, Text: errText}}:
			case <-ctx.Done():
			}
			outcomeCh <- agent.AgentOutcome{Err: classifyLaunchErr(serr)}
			return
		}

		// Terminal display.
		summary := liveEventText(tokenSummary(usage))
		select {
		case events <- agent.AgentEvent{Kind: "done", Stream: "stdout", Live: true, Payload: []byte(summary), Display: agent.EventDisplay{Class: agent.DisplayFinal, Text: summary}}:
		case <-ctx.Done():
		}

		// Empty output, refusal, or truncation → retryable unparseable.
		if full == "" || finish == "length" {
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrUnparseableOutput{NodePath: inv.NodePath}}
			return
		}
		res, berr := buildResult(full, usage, wireModel, a.pricer, inv)
		if berr != nil {
			outcomeCh <- agent.AgentOutcome{Err: berr} // ErrUnparseableOutput (retryable)
			return
		}
		outcomeCh <- agent.AgentOutcome{Result: res}
	}()

	return events, outcomeCh, nil
}

func liveEventText(s string) string {
	return agent.RedactDisplayText(agent.SanitizeDisplayText(s))
}

// classifyLaunchErr maps a transport/stream error to a mechanical class:
// permanent (*agent.ErrInvalidConfig) iff a 400 invalid_request_error or
// quota/budget exhaustion; else retryable (*agent.ErrAgentLaunch). The
// x-should-retry header, when present, is AUTHORITATIVE and overrides the
// status-based decision either way. A retryable apiError carrying a parsed
// Retry-After is surfaced with an agent.RetryHint so the engine honors the
// server's window instead of the short exp curve.
func classifyLaunchErr(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		if ae.ShouldRetry != nil {
			if !*ae.ShouldRetry {
				return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "", Reason: err.Error()}
			}
			return &agent.ErrAgentLaunch{Cause: err, RetryHint: retryHintFromAPIErr(ae)}
		}
		if isPermanentLLMError(err) {
			return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "", Reason: err.Error()}
		}
		return &agent.ErrAgentLaunch{Cause: err, RetryHint: retryHintFromAPIErr(ae)}
	}
	// Non-apiError (raw transport/connection fault, ctx error) → retryable, no hint.
	return &agent.ErrAgentLaunch{Cause: err} // agent.ErrAgentLaunch is {Cause, RetryHint} (agent/errors.go)
}

// retryHintFromAPIErr lifts a parsed Retry-After onto an agent.RetryHint, or nil
// when the server gave no usable wait window.
func retryHintFromAPIErr(ae *apiError) *agent.RetryHint {
	if ae.RetryAfter > 0 {
		return &agent.RetryHint{RetryAfter: ae.RetryAfter}
	}
	return nil
}
