package awfllm_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/awfllm"
)

func TestParseRetrySignals(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("retry-after seconds", func(t *testing.T) {
		h := http.Header{"Retry-After": {"30"}}
		ra, sr := awfllm.ParseRetrySignalsForTest(h, now)
		if ra != 30*time.Second {
			t.Errorf("RetryAfter = %v, want 30s", ra)
		}
		if sr != nil {
			t.Errorf("ShouldRetry = %v, want nil", *sr)
		}
	})

	t.Run("retry-after-ms takes priority over retry-after", func(t *testing.T) {
		h := http.Header{"Retry-After": {"30"}, "Retry-After-Ms": {"1500"}}
		ra, _ := awfllm.ParseRetrySignalsForTest(h, now)
		if ra != 1500*time.Millisecond {
			t.Errorf("RetryAfter = %v, want 1.5s (ms header wins)", ra)
		}
	})

	t.Run("retry-after http-date", func(t *testing.T) {
		h := http.Header{"Retry-After": {now.Add(45 * time.Second).UTC().Format(http.TimeFormat)}}
		ra, _ := awfllm.ParseRetrySignalsForTest(h, now)
		if ra != 45*time.Second {
			t.Errorf("RetryAfter = %v, want 45s (HTTP-date relative to now)", ra)
		}
	})

	t.Run("x-should-retry true", func(t *testing.T) {
		h := http.Header{"X-Should-Retry": {"true"}}
		_, sr := awfllm.ParseRetrySignalsForTest(h, now)
		if sr == nil || !*sr {
			t.Errorf("ShouldRetry = %v, want &true", sr)
		}
	})

	t.Run("x-should-retry false", func(t *testing.T) {
		h := http.Header{"X-Should-Retry": {"false"}}
		_, sr := awfllm.ParseRetrySignalsForTest(h, now)
		if sr == nil || *sr {
			t.Errorf("ShouldRetry = %v, want &false", sr)
		}
	})

	t.Run("no headers", func(t *testing.T) {
		ra, sr := awfllm.ParseRetrySignalsForTest(http.Header{}, now)
		if ra != 0 || sr != nil {
			t.Errorf("got (%v, %v), want (0, nil)", ra, sr)
		}
	})
}

func TestClassifyLaunchErr_HintAndOverride(t *testing.T) {
	t.Run("429 rate_limit with retry-after → retryable carrying the hint", func(t *testing.T) {
		ae := awfllm.NewAPIErrorWithHintForTest(429, "rate_limit_error", 30*time.Second, nil)
		out := awfllm.ClassifyLaunchErrForTest(ae)
		var la *agent.ErrAgentLaunch
		if !errors.As(out, &la) {
			t.Fatalf("out = %v (%T), want *agent.ErrAgentLaunch (429 is retryable)", out, out)
		}
		if la.RetryHint == nil || la.RetryHint.RetryAfter != 30*time.Second {
			t.Errorf("RetryHint = %+v, want RetryAfter=30s", la.RetryHint)
		}
	})

	t.Run("400 invalid_request → permanent (unchanged)", func(t *testing.T) {
		ae := awfllm.NewAPIErrorWithHintForTest(400, "invalid_request_error", 0, nil)
		out := awfllm.ClassifyLaunchErrForTest(ae)
		var bad *agent.ErrInvalidConfig
		if !errors.As(out, &bad) {
			t.Fatalf("out = %v (%T), want *agent.ErrInvalidConfig", out, out)
		}
	})

	t.Run("401 authentication → permanent (invalid key fails fast)", func(t *testing.T) {
		ae := awfllm.NewAPIErrorWithHintForTest(401, "authentication_error", 0, nil)
		out := awfllm.ClassifyLaunchErrForTest(ae)
		var bad *agent.ErrInvalidConfig
		if !errors.As(out, &bad) {
			t.Fatalf("out = %v (%T), want permanent (a present-but-invalid key is deterministic)", out, out)
		}
	})

	t.Run("403 permission → permanent", func(t *testing.T) {
		ae := awfllm.NewAPIErrorWithHintForTest(403, "permission_error", 0, nil)
		out := awfllm.ClassifyLaunchErrForTest(ae)
		var bad *agent.ErrInvalidConfig
		if !errors.As(out, &bad) {
			t.Fatalf("out = %v (%T), want permanent (403)", out, out)
		}
	})

	t.Run("429 with x-should-retry:true overrides 401-style permanence", func(t *testing.T) {
		// Authoritative override still wins even over a normally-permanent status.
		tr := true
		ae := awfllm.NewAPIErrorWithHintForTest(401, "authentication_error", 0, &tr)
		out := awfllm.ClassifyLaunchErrForTest(ae)
		var la *agent.ErrAgentLaunch
		if !errors.As(out, &la) {
			t.Fatalf("out = %v (%T), want retryable (x-should-retry:true is authoritative)", out, out)
		}
	})
}

// TestClassifyOpenAIErr_ForwardsHeaders verifies the OpenAI-compat error path
// (classifyOpenAIErr) forwards the response Retry-After / x-should-retry headers
// just like the Ollama/Gemini/Anthropic paths — otherwise a 429 from an
// OpenAI-compatible gateway (incl. a LiteLLM-proxied Claude) drops its hint.
func TestClassifyOpenAIErr_ForwardsHeaders(t *testing.T) {
	// The openai SDK populates Request+Response on every *openai.Error it returns
	// (Error() formats from both), so a realistic error carries them.
	oeReq, _ := http.NewRequest(http.MethodPost, "https://api.example/v1/chat/completions", nil)

	t.Run("retry-after forwarded", func(t *testing.T) {
		oe := &openai.Error{
			StatusCode: 429,
			Type:       "rate_limit_error",
			Request:    oeReq,
			Response:   &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"30"}}},
		}
		out := awfllm.ClassifyLaunchErrForTest(awfllm.ClassifyOpenAIErrForTest(oe))
		var la *agent.ErrAgentLaunch
		if !errors.As(out, &la) {
			t.Fatalf("out = %v (%T), want *agent.ErrAgentLaunch", out, out)
		}
		if la.RetryHint == nil || la.RetryHint.RetryAfter != 30*time.Second {
			t.Errorf("RetryHint = %+v, want RetryAfter=30s (OpenAI path must forward Retry-After)", la.RetryHint)
		}
	})

	t.Run("x-should-retry:false forwarded → permanent", func(t *testing.T) {
		oe := &openai.Error{
			StatusCode: 429,
			Type:       "rate_limit_error",
			Request:    oeReq,
			Response:   &http.Response{StatusCode: 429, Header: http.Header{"X-Should-Retry": {"false"}}},
		}
		out := awfllm.ClassifyLaunchErrForTest(awfllm.ClassifyOpenAIErrForTest(oe))
		var bad *agent.ErrInvalidConfig
		if !errors.As(out, &bad) {
			t.Fatalf("out = %v (%T), want permanent (x-should-retry:false from OpenAI path)", out, out)
		}
	})

	t.Run("x-should-retry:false on a 429 → permanent override", func(t *testing.T) {
		f := false
		ae := awfllm.NewAPIErrorWithHintForTest(429, "rate_limit_error", 30*time.Second, &f)
		out := awfllm.ClassifyLaunchErrForTest(ae)
		var bad *agent.ErrInvalidConfig
		if !errors.As(out, &bad) {
			t.Fatalf("out = %v (%T), want permanent (x-should-retry:false must suppress retry)", out, out)
		}
	})

	t.Run("x-should-retry:true on a 400 → retryable override", func(t *testing.T) {
		tr := true
		ae := awfllm.NewAPIErrorWithHintForTest(400, "invalid_request_error", 0, &tr)
		out := awfllm.ClassifyLaunchErrForTest(ae)
		var la *agent.ErrAgentLaunch
		if !errors.As(out, &la) {
			t.Fatalf("out = %v (%T), want retryable (x-should-retry:true must force retry)", out, out)
		}
	})
}
