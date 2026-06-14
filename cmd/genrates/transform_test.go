package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// a minimal models.dev-shaped document covering the cases we transform.
const sampleModelsDev = `{
  "openai": {"models": {
    "gpt-x": {"cost": {"input": 1.25, "output": 10, "cache_read": 0.125}, "last_updated": "2026-02-05"}
  }},
  "anthropic": {"models": {
    "claude-z": {"cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25}, "last_updated": "2026-05-28"}
  }},
  "google": {"models": {
    "gemini-tiered": {"cost": {"input": 1.25, "output": 10, "cache_read": 0.125, "input_audio": 1,
      "tiers": [{"input": 2.5, "output": 15}], "context_over_200k": {"input": 2.5}}, "last_updated": "2025-06-05"},
    "gemini-free": {"cost": {"input": 0, "output": 0}, "last_updated": "2025-01-01"}
  }},
  "aggregator": {"models": {
    "gpt-x": {"cost": {"input": 99, "output": 99}, "last_updated": "2020-01-01"}
  }}
}`

var firstPartyForTest = []string{"openai", "anthropic", "google"}

func TestLookupModelsDev_PerMillionPassthroughAndDefaults(t *testing.T) {
	doc, err := parseModelsDev([]byte(sampleModelsDev))
	if err != nil {
		t.Fatal(err)
	}
	r, ok, err := lookupModelsDev(doc, firstPartyForTest, "gpt-x")
	if err != nil || !ok {
		t.Fatalf("lookup gpt-x: ok=%v err=%v", ok, err)
	}
	if r.InputPerM != 1.25 || r.OutputPerM != 10 || r.CacheReadPerM != 0.125 {
		t.Errorf("rates = %+v", r)
	}
	if r.CacheWritePerM != 0 {
		t.Errorf("absent cache_write must default to 0, got %v", r.CacheWritePerM)
	}
	if r.Currency != "USD" || r.UpdatedOn != "2026-02-05" || r.Source != sourceURL {
		t.Errorf("meta wrong: %+v", r)
	}
}

func TestLookupModelsDev_TieredTakesBaseRateOnly(t *testing.T) {
	doc, _ := parseModelsDev([]byte(sampleModelsDev))
	r, ok, _ := lookupModelsDev(doc, firstPartyForTest, "gemini-tiered")
	if !ok {
		t.Fatal("expected hit")
	}
	// Base rate at the top of cost{} — tiers / context_over_200k / input_audio dropped.
	if r.InputPerM != 1.25 || r.OutputPerM != 10 || r.CacheReadPerM != 0.125 {
		t.Errorf("tiered model must use BASE rate, got %+v", r)
	}
}

func TestLookupModelsDev_FirstPartyPriorityBeatsAggregator(t *testing.T) {
	doc, _ := parseModelsDev([]byte(sampleModelsDev))
	r, ok, _ := lookupModelsDev(doc, firstPartyForTest, "gpt-x")
	if !ok || r.InputPerM != 1.25 {
		t.Fatalf("must pick the first-party openai entry (1.25), not the aggregator (99): %+v", r)
	}
}

func TestLookupModelsDev_FailClosedOnNonPositive(t *testing.T) {
	doc, _ := parseModelsDev([]byte(sampleModelsDev))
	if _, _, err := lookupModelsDev(doc, firstPartyForTest, "gemini-free"); err == nil {
		t.Fatal("a model with input/output 0 must fail-closed, not emit a $0 entry")
	}
}

func TestLookupModelsDev_MissReturnsFalse(t *testing.T) {
	doc, _ := parseModelsDev([]byte(sampleModelsDev))
	if _, ok, err := lookupModelsDev(doc, firstPartyForTest, "no-such-model"); ok || err != nil {
		t.Fatalf("miss must be ok=false, no error: ok=%v err=%v", ok, err)
	}
}

func TestLookupLiteLLM_PerTokenScaledToPerMillion(t *testing.T) {
	const raw = `{"gpt-x":{"input_cost_per_token":1.25e-06,"output_cost_per_token":1e-05,
	  "cache_read_input_token_cost":1.25e-07,"cache_creation_input_token_cost":1.5625e-06}}`
	r, ok, err := lookupLiteLLM([]byte(raw), "gpt-x", "2026-06-14")
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if r.InputPerM != 1.25 || r.OutputPerM != 10 || r.CacheReadPerM != 0.125 || r.CacheWritePerM != 1.5625 {
		t.Errorf("per-token *1e6 wrong: %+v", r)
	}
	if r.UpdatedOn != "2026-06-14" || r.Source != liteLLMSourceURL {
		t.Errorf("meta wrong (litellm has no per-model date → sync date): %+v", r)
	}
}

func TestBuild_FallsBackToLiteLLMThenFailsClosed(t *testing.T) {
	doc := []byte(sampleModelsDev)
	litellm := []byte(`{"only-in-litellm":{"input_cost_per_token":1e-06,"output_cost_per_token":2e-06}}`)
	called := 0
	fetchLite := func() ([]byte, error) { called++; return litellm, nil }

	// gpt-x resolves from models.dev; only-in-litellm from the fallback.
	tbl, err := build(doc, []string{"gpt-x", "only-in-litellm"}, firstPartyForTest, "2026-06-14", fetchLite)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tbl["only-in-litellm"]; !ok {
		t.Error("litellm fallback entry missing")
	}
	if called != 1 {
		t.Errorf("litellm should be fetched exactly once (lazy), got %d", called)
	}

	// A model in neither source must fail-closed (never silently dropped).
	if _, err := build(doc, []string{"ghost-model"}, firstPartyForTest, "2026-06-14", fetchLite); err == nil {
		t.Fatal("a model absent from both sources must fail-closed")
	}
}

func TestRenderStableSortedJSON(t *testing.T) {
	doc, _ := parseModelsDev([]byte(sampleModelsDev))
	tbl, err := build([]byte(sampleModelsDev), []string{"claude-z", "gpt-x"}, firstPartyForTest, "2026-06-14", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := render(tbl)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.HasSuffix(s, "\n") {
		t.Error("output must end with a trailing newline")
	}
	// Sorted keys: claude-z before gpt-x.
	if strings.Index(s, "claude-z") > strings.Index(s, "gpt-x") {
		t.Error("keys must be sorted (claude-z before gpt-x)")
	}
	// Round-trips to the same table (sanity).
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("rendered JSON does not parse: %v", err)
	}
	_ = doc
}
