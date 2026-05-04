package claude

// ShellQuoteForTest exposes the package-private shellQuote helper to
// external tests in package claude_test. Lives in _test.go so it ships
// only with the test binary.
var ShellQuoteForTest = shellQuote
