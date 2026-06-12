package pricing

import (
	"sort"
	"time"
)

const dateLayout = "2006-01-02"

// Stale returns a sorted list of model IDs whose UpdatedOn is empty,
// unparseable, or older than maxAge relative to asOf.
func Stale(t Table, asOf time.Time, maxAge time.Duration) []string {
	var stale []string
	for id, r := range t {
		if r.UpdatedOn == "" {
			stale = append(stale, id)
			continue
		}
		updated, err := time.Parse(dateLayout, r.UpdatedOn)
		if err != nil {
			stale = append(stale, id)
			continue
		}
		if asOf.Sub(updated) > maxAge {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	return stale
}
