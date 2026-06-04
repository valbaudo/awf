package codex

// Exports for whitebox tests (test files are package codex_test).
var (
	ParseStreamEventForTest      = parseStreamEvent
	BuildResultForTest           = buildResult
	IsPermanentCodexErrorForTest = isPermanentCodexError
	DisplayForCodexForTest       = displayForCodex
	AgentMessageTextForTest      = agentMessageText
	EventKindForTest             = eventKind
)

// NewUsageForTest builds a *usageRec for buildResult token-fill tests (usageRec
// is unexported). in/cached/out → input_tokens/cached_input_tokens/output_tokens.
func NewUsageForTest(in, cached, out int) *usageRec {
	return &usageRec{InputTokens: in, CachedInputTokens: cached, OutputTokens: out}
}
