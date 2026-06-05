package awfllm

import (
	"context"

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
	prompt := assemblePrompt(inv)

	events := make(chan agent.AgentEvent, eventsBuffer)
	outcomeCh := make(chan agent.AgentOutcome, 1)

	go func() {
		// LIFO: events closes before outcomeCh (documented order).
		defer close(outcomeCh)
		defer close(events)

		emit := func(delta string, raw []byte) {
			ev := agent.AgentEvent{
				Kind:    "delta",
				Stream:  "stdout",
				Payload: raw, // already a fresh copy from transport
				// RAW delta text — NOT agent.Elide (Elide clips a >512B line with an
				// ellipsis marker, which would corrupt the live stream). DisplayAssistantDelta
				// (Task A7) makes the renderer write it WITHOUT a trailing newline → the
				// deltas concatenate character-by-character.
				Display: agent.EventDisplay{Class: agent.DisplayAssistantDelta, Text: delta},
			}
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		}

		full, usage, finish, serr := a.stream(ctx, cfg, prompt, inv.OutputSchema, emit)
		if serr != nil {
			// spec §B.7 step 4: emit a terminal DisplayError event before the
			// outcome so the live renderer can terminate the in-progress delta
			// line and display the error prominently. The ctx-aware send mirrors
			// the emit helper above — if the context is already done we skip the
			// event but still send the outcome below.
			select {
			case events <- agent.AgentEvent{Kind: "error", Stream: "stderr", Display: agent.EventDisplay{Class: agent.DisplayError, IsError: true, Text: serr.Error()}}:
			case <-ctx.Done():
			}
			outcomeCh <- agent.AgentOutcome{Err: classifyLaunchErr(serr)}
			return
		}

		// Terminal display.
		select {
		case events <- agent.AgentEvent{Kind: "done", Stream: "stdout", Display: agent.EventDisplay{Class: agent.DisplayFinal, Text: tokenSummary(usage)}}:
		case <-ctx.Done():
		}

		// Empty output, refusal, or truncation → retryable unparseable.
		if full == "" || finish == "length" {
			outcomeCh <- agent.AgentOutcome{Err: &agent.ErrUnparseableOutput{NodePath: inv.NodePath}}
			return
		}
		res, berr := buildResult(full, usage, inv)
		if berr != nil {
			outcomeCh <- agent.AgentOutcome{Err: berr} // ErrUnparseableOutput (retryable)
			return
		}
		outcomeCh <- agent.AgentOutcome{Result: res}
	}()

	return events, outcomeCh, nil
}

// classifyLaunchErr maps a transport/stream error to a mechanical class:
// permanent (*agent.ErrInvalidConfig) iff a 400 invalid_request_error; else
// retryable (*agent.ErrAgentLaunch).
func classifyLaunchErr(err error) error {
	if isPermanentLLMError(err) {
		return &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "", Reason: err.Error()}
	}
	return &agent.ErrAgentLaunch{Cause: err} // agent.ErrAgentLaunch is {Cause error} ONLY — no Ref field (agent/errors.go:62)
}
