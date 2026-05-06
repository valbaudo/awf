package obs

import "github.com/valbaudo/awf/engine"

// Attribute names. awf.* is the STABLE contract (never renamed). gen_ai.* names
// follow OTel semantic-conventions spec v1.41.1 and are hand-defined here as raw
// strings because the Go semconv package lags (v1.37.0 at otel v1.43.0) and does
// not define gen_ai.evaluation.*. This file is the single swappable mapping
// layer: a convention bump is a one-file edit.
const (
	AttrWorkflowID      = "awf.workflow.id"
	AttrWorkflowVersion = "awf.workflow.version"
	AttrWorkflowDigest  = "awf.workflow.digest"
	AttrRunID           = "awf.run.id"
	AttrRunEpoch        = "awf.run.epoch"
	AttrNodePath        = "awf.node.path"
	AttrNodeKind        = "awf.node.kind"  // leaf step spans only — one of the 10 real node kinds
	AttrScopeKind       = "awf.scope.kind" // synthesized control-scope spans only — structural role (M1)
	AttrNodeOutcome     = "awf.node.outcome"
	AttrExitCode        = "awf.exit_code"
	AttrAgentTurns      = "awf.agent.turns"
	AttrBranch          = "awf.branch"
	AttrLoopIterations  = "awf.loop.iterations"
	AttrSkipReason      = "awf.skip.reason"
	AttrGateAttempt     = "awf.gate.attempt"
	AttrGateStatus      = "awf.gate.status"
	AttrCostUSD         = "awf.cost.usd"
	AttrCostSource      = "awf.cost.source"
	AttrGateAttempts    = "awf.gate.attempts"
	AttrGateOutcome     = "awf.gate.outcome"
	AttrRunCostUSD      = "awf.run.cost.usd"

	AttrGenAIInputTokens  = "gen_ai.usage.input_tokens"
	AttrGenAIOutputTokens = "gen_ai.usage.output_tokens"
	AttrGenAICacheRead    = "gen_ai.usage.cache_read.input_tokens"
	AttrGenAICacheCreate  = "gen_ai.usage.cache_creation.input_tokens"
	AttrGenAIConversation = "gen_ai.conversation.id"
	AttrSessionID         = "session.id"

	// gen_ai.evaluation.result event (Task 11). Not in the Go semconv package.
	EventGenAIEvaluation = "gen_ai.evaluation.result"
	AttrGenAIEvalName    = "gen_ai.evaluation.name"

	// outcomeIncomplete is the AttrNodeOutcome value on a Pending span (Task 9).
	outcomeIncomplete = "incomplete"
	outcomeSkipped    = "skipped"
)

// stepAttributes builds the attribute map for a leaf step span from its
// node.completed payload. Cost/token attrs are emitted ONLY when metrics are
// present (decision 4: absent cost is omitted, never a misleading 0). Values
// are restricted to string / int64 / float64 / bool (Export maps each).
func stepAttributes(path, kind string, nc engine.NodeCompletedData) map[string]any {
	attrs := map[string]any{
		AttrNodePath:    path,
		AttrNodeKind:    kind,
		AttrNodeOutcome: nc.Outcome,
	}
	if nc.ExitCode != nil {
		attrs[AttrExitCode] = int64(*nc.ExitCode)
	}
	// gen_ai.* + cost attrs only for agent steps (m4): MetricSet is produced
	// solely by agent adapters today, but gating on kind keeps gen_ai.* off
	// non-LLM spans regardless of any future metrics source.
	if kind == "agent" && nc.Metrics != nil {
		m := nc.Metrics
		if m.Cost.Source != "" {
			attrs[AttrCostUSD] = m.Cost.USD
			attrs[AttrCostSource] = m.Cost.Source
		}
		attrs[AttrGenAIInputTokens] = int64(m.Tokens.Input)
		attrs[AttrGenAIOutputTokens] = int64(m.Tokens.Output)
		if m.Tokens.CacheReadInput > 0 {
			attrs[AttrGenAICacheRead] = int64(m.Tokens.CacheReadInput)
		}
		if m.Tokens.CacheCreationInput > 0 {
			attrs[AttrGenAICacheCreate] = int64(m.Tokens.CacheCreationInput)
		}
		attrs[AttrAgentTurns] = int64(m.Turns)
	}
	return attrs
}
