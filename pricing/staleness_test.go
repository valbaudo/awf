package pricing

import (
	"testing"
	"time"
)

func TestStaleFlagsOldEntries(t *testing.T) {
	asOf := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	tb := Table{
		"fresh":   {UpdatedOn: "2026-06-01"},
		"old":     {UpdatedOn: "2025-01-01"},
		"undated": {},
	}
	got := Stale(tb, asOf, 90*24*time.Hour)
	if !contains(got, "old") || !contains(got, "undated") || contains(got, "fresh") {
		t.Fatalf("Stale = %v", got)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
