package codexlive

import (
	"testing"

	"github.com/valbaudo/awf/pricing"
)

// normalizeForPricing uses totalTokens as a runtime oracle for cache inclusion.
// Codex's inputTokens is BELIEVED to include cachedInputTokens (cached ⊂ input),
// but the app-server schema doesn't prove it. So: subtract cached from input IFF
// totalTokens == inputTokens + outputTokens (the inclusion identity). Otherwise
// (e.g. totalTokens == input + cached + output, the disjoint case) leave input
// whole — subtracting would understate the priced input.
func TestNormalizeForPricing(t *testing.T) {
	t.Run("inclusion subtracts cached", func(t *testing.T) {
		// 1000 + 340 == 1340 → cached ⊂ input → subtract.
		b, subtracted := normalizeForPricing(Usage{InputTokens: 1000, OutputTokens: 340, CachedInputTokens: 800, TotalTokens: 1340})
		if !subtracted {
			t.Fatal("subtracted = false, want true (inclusion: 1000+340==1340)")
		}
		want := pricing.Breakdown{Input: 200, Output: 340, CacheRead: 800}
		if b != want {
			t.Fatalf("Breakdown = %+v, want %+v", b, want)
		}
	})

	t.Run("disjoint does not subtract", func(t *testing.T) {
		// 1000 + 800 + 340 == 2140 → cached disjoint from input → DO NOT subtract.
		b, subtracted := normalizeForPricing(Usage{InputTokens: 1000, OutputTokens: 340, CachedInputTokens: 800, TotalTokens: 2140})
		if subtracted {
			t.Fatal("subtracted = true, want false (disjoint: total != input+output)")
		}
		want := pricing.Breakdown{Input: 1000, Output: 340, CacheRead: 800}
		if b != want {
			t.Fatalf("Breakdown = %+v, want %+v", b, want)
		}
	})
}
