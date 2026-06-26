package claude

// ShellQuoteForTest exposes the package-private shellQuote helper to
// external tests in package claude_test. Lives in _test.go so it ships
// only with the test binary.
var ShellQuoteForTest = shellQuote

// AdapterEnvForTest returns a copy of the adapter's internal env map for
// mutation-detection tests. The copy prevents callers from mutating adapter
// state through the returned map.
func AdapterEnvForTest(a *Adapter) map[string]string {
	out := make(map[string]string, len(a.env))
	for k, v := range a.env {
		out[k] = v
	}
	return out
}
