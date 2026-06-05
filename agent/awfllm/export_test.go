package awfllm

// Exports for white-box tests (test files are package awfllm_test).
var (
	ExtractJSONObjectForTest   = extractJSONObject
	BuildResultForTest         = buildResult
	IsPermanentLLMErrorForTest = isPermanentLLMError
)

func NewUsageForTest(in, out, cached int) usageRec {
	return usageRec{Input: in, Output: out, CacheRead: cached}
}

func NewAPIErrorForTest(status int, typ, body string) *apiError {
	return &apiError{Status: status, Type: typ, Body: body}
}
