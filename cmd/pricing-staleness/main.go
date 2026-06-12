package main

import (
	"fmt"
	"time"

	"github.com/valbaudo/awf/pricing"
)

func main() {
	stale := pricing.Stale(pricing.Default(), time.Now().UTC(), 90*24*time.Hour)
	if len(stale) == 0 {
		fmt.Println("pricing: all rate entries are fresh (<90d)")
		return
	}
	fmt.Println("pricing: stale or undated rate entries (refresh + re-vendor pricing/rates.json):")
	for _, m := range stale {
		fmt.Println("  " + m)
	}
	// report only — always exit 0
}
