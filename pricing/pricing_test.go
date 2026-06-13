package pricing

import "testing"

func tbl() Table {
	return Table{"gpt-5-codex": {Currency: "USD", InputPerM: 1.25, OutputPerM: 10, CacheReadPerM: 0.125, CacheWritePerM: 0}}
}

func TestDeriveTotalIsSumOfParts(t *testing.T) {
	c, ok := tbl().Derive("gpt-5-codex", Breakdown{Input: 1_000_000, Output: 1_000_000, CacheRead: 0})
	if !ok {
		t.Fatal("expected hit")
	}
	if c.Input != 1.25 || c.Output != 10 {
		t.Fatalf("parts = %+v", c)
	}
	if c.Total != c.Input+c.Output {
		t.Fatalf("Total %v != Input+Output %v", c.Total, c.Input+c.Output)
	}
}

func TestDeriveCacheFoldedIntoInput(t *testing.T) {
	c, _ := tbl().Derive("gpt-5-codex", Breakdown{Input: 0, Output: 0, CacheRead: 1_000_000})
	if c.Input != 0.125 {
		t.Fatalf("cache-read should fold into Input: %+v", c)
	}
}

func TestDeriveLadderStripsProviderRegionDateBedrock(t *testing.T) {
	tb := Table{"claude-opus-4-8": {Currency: "USD", InputPerM: 5, OutputPerM: 25, CacheReadPerM: 0.5, CacheWritePerM: 6.25}}
	for _, id := range []string{
		"anthropic/claude-opus-4-8",
		"us.anthropic.claude-opus-4-8",
		"anthropic.claude-opus-4-8",
		"claude-opus-4-8-20991231",
	} {
		if _, ok := tb.Derive(id, Breakdown{Input: 1_000_000}); !ok {
			t.Errorf("ladder failed to match %q", id)
		}
	}
}

func TestDeriveMissReturnsFalseNotZero(t *testing.T) {
	if c, ok := tbl().Derive("totally-unknown-model", Breakdown{Input: 999}); ok || c.Total != 0 {
		t.Fatalf("miss must be ok=false (got ok=%v cost=%+v)", ok, c)
	}
	if _, ok := tbl().Derive("arn:aws:bedrock:us-east-1:1234:inference-profile/x", Breakdown{Input: 999}); ok {
		t.Fatal("ARN must not match")
	}
}

func TestDeriveEmptyCurrencyNotMaterialized(t *testing.T) {
	tb := Table{"m": {Currency: "", InputPerM: 1, OutputPerM: 1}}
	c, ok := tb.Derive("m", Breakdown{Input: 1, Output: 1})
	if !ok {
		t.Fatal("Derive: ok=false")
	}
	if c.Currency != "" {
		t.Errorf("Currency = %q, want \"\" (empty == USD by contract, not materialized)", c.Currency)
	}
}
