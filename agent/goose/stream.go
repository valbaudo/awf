package goose

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/pricing"
)

// streamEvent is one line of goose's `--output-format stream-json` NDJSON. Three
// shapes: "message" (assistant text — an INCREMENTAL delta, not a snapshot),
// terminal "complete" (token totals), and "error" (a stream/provider fault).
type streamEvent struct {
	Type         string         `json:"type"`
	Message      *streamMessage `json:"message,omitempty"`
	InputTokens  int            `json:"input_tokens,omitempty"`
	OutputTokens int            `json:"output_tokens,omitempty"`
	Error        string         `json:"error,omitempty"`
}

type streamMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseStreamEvent(b []byte) (*streamEvent, error) {
	var ev streamEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, &ErrStreamParse{Line: b, Cause: err}
	}
	return &ev, nil
}

// assistantText returns the concatenated text of a "message" event IFF it is
// role=="assistant" (else ""). goose streams assistant output as incremental
// deltas, so Launch concatenates assistantText across lines in arrival order. The
// role filter keeps tool/user echoes (e.g. the final_output continuation's user
// message) out of the accumulated final text, so a {...} in a non-assistant line
// can never be mistaken for the answer.
func assistantText(ev *streamEvent) string {
	if ev.Type != "message" || ev.Message == nil || ev.Message.Role != "assistant" {
		return ""
	}
	var b strings.Builder
	for _, blk := range ev.Message.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// buildResult builds the AgentResult from the fully-reassembled final assistant
// text and the terminal "complete" event. With a schema, extracts a JSON object
// (layer-2); non-parseable → *agent.ErrUnparseableOutput. Without a schema, Output
// is nil.
//
// Cost is DERIVED from tokens via pricer (the adapter's injected pricing.Table —
// never the global pricing.Default() here, so tests can swap a fixture table).
// goose's harness emits no model id, so we price on the REQUESTED with:{model}:
// unset/empty model OR a model the table doesn't know → cost ABSENT (Source==""),
// which is correct, not $0. goose reports no cache fields, so Input is passed to
// the pricing package as-is (no cache subset to subtract).
func buildResult(finalText string, complete *streamEvent, inv agent.AgentInvocation, pricer pricing.Table) (agent.AgentResult, error) {
	var output map[string]any
	if inv.OutputSchema != nil {
		parsed, perr := extractJSONObject(finalText)
		if perr != nil {
			return agent.AgentResult{}, &agent.ErrUnparseableOutput{NodePath: inv.NodePath}
		}
		output = parsed
	}
	var tokens agent.MetricTokens
	if complete != nil {
		tokens.Input = complete.InputTokens
		tokens.Output = complete.OutputTokens
	}
	model, _ := inv.With[keyModel].(string)
	ms := agent.MetricSet{Tokens: tokens, Model: model}
	if model != "" {
		b := pricing.Breakdown{Input: tokens.Input, Output: tokens.Output} // goose has no cache subset
		if c, ok := pricer.Derive(model, b); ok {
			ms.Cost = agent.MetricCost{Source: agent.CostSourceDerived, Currency: c.Currency, Total: c.Total, Input: c.Input, Output: c.Output}
		}
	}
	return agent.AgentResult{Output: output, ExitCode: 0, Metrics: ms}, nil
}

// extractJSONObject delegates to the shared agent.ExtractJSONObject (F37 —
// hoisted from three byte-identical per-adapter copies; see agent/schema.go).
// Kept as a local name (rather than calling agent.ExtractJSONObject inline)
// because it is exported for white-box tests via export_test.go.
func extractJSONObject(s string) (map[string]any, error) {
	return agent.ExtractJSONObject(s)
}

// displayForGoose maps one goose stream-json event to agent.EventDisplay.
func displayForGoose(ev *streamEvent) agent.EventDisplay {
	switch ev.Type {
	case "message":
		if t := assistantText(ev); t != "" {
			return agent.EventDisplay{Class: agent.DisplayAssistant, Text: t}
		}
		return agent.EventDisplay{} // non-assistant message → Other (terse)
	case "complete":
		return agent.EventDisplay{Class: agent.DisplayFinal, Text: fmt.Sprintf("%d in / %d out tokens", ev.InputTokens, ev.OutputTokens)}
	case "error":
		return agent.EventDisplay{Class: agent.DisplayError, IsError: true, Text: ev.Error}
	default:
		return agent.EventDisplay{}
	}
}
