package main

import (
	"fmt"
	"time"

	"github.com/valbaudo/awf/pricing"
)

func main() {
	// updated_on records when a price was last REVISED upstream (it comes from
	// models.dev's per-model last_updated via cmd/genrates), NOT when AWF last
	// vendored the table — so this report is advisory, not an action item: a price
	// untouched for a long time is usually still correct, just worth a glance.
	// The threshold is a price-REVISION cadence (1 year), not a vendoring cadence;
	// re-running `make pricing-regen` will NOT clear an entry whose upstream date
	// is genuinely old. (Undated entries always flag — those are real gaps.)
	const maxAge = 365 * 24 * time.Hour
	stale := pricing.Stale(pricing.Default(), time.Now().UTC(), maxAge)
	if len(stale) == 0 {
		fmt.Println("pricing: all rate entries were revised upstream within the last year")
		return
	}
	fmt.Println("pricing: rate entries not revised upstream in >1y (or undated) — verify they are still current at each model's source:")
	for _, m := range stale {
		fmt.Println("  " + m)
	}
	// report only — always exit 0
}
