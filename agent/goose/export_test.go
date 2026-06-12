package goose

// Exports for whitebox tests (test files are package goose_test).
var (
	AssistantTextForTest     = assistantText
	ExtractJSONObjectForTest = extractJSONObject
	ParseStreamEventForTest  = parseStreamEvent
	DisplayForGooseForTest   = displayForGoose
)

// BuildResultForTest exposes buildResult with its injected pricer param so
// whitebox tests can drive cost derivation off a fixture pricing.Table (pass nil
// to exercise the no-pricing path → cost ABSENT).
var BuildResultForTest = buildResult

// NewCompleteForTest builds a terminal "complete" *streamEvent (unexported) with
// the given token totals for buildResult cost tests.
func NewCompleteForTest(in, out int) *streamEvent {
	return &streamEvent{Type: "complete", InputTokens: in, OutputTokens: out}
}

var (
	ShellQuoteForTest      = shellQuote
	IsConfigErrorForTest   = isConfigError
	AssembleCommandForTest = assembleCommand
)
