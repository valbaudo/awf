package codex

// Exports for whitebox tests (test files are package codex_test).
var (
	ParseStreamEventForTest      = parseStreamEvent
	IsPermanentCodexErrorForTest = isPermanentCodexError
	DisplayForCodexForTest       = displayForCodex
	AgentMessageTextForTest      = agentMessageText
	EventKindForTest             = eventKind
)

// BuildResultForTest exposes buildResult with its injected pricer param so
// whitebox tests can drive cost derivation off a fixture pricing.Table (pass nil
// to exercise the no-pricing path → cost ABSENT).
var BuildResultForTest = buildResult

// NewUsageForTest builds a *usageRec for buildResult token-fill tests (usageRec
// is unexported). in/cached/out → input_tokens/cached_input_tokens/output_tokens.
func NewUsageForTest(in, cached, out int) *usageRec {
	return &usageRec{InputTokens: in, CachedInputTokens: cached, OutputTokens: out}
}

var (
	ShellQuoteForTest      = shellQuote
	AssembleCommandForTest = assembleCommand
)
