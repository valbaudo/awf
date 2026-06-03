package goose

// Exports for whitebox tests (test files are package goose_test).
var (
	AssistantTextForTest     = assistantText
	ExtractJSONObjectForTest = extractJSONObject
	ParseStreamEventForTest  = parseStreamEvent
	DisplayForGooseForTest   = displayForGoose
)

var (
	ShellQuoteForTest      = shellQuote
	IsConfigErrorForTest   = isConfigError
	AssembleCommandForTest = assembleCommand
)
