package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
)

//go:embed rates.json
var embeddedRates []byte

var (
	defaultOnce  sync.Once
	defaultTable Table
)

// Default returns embedded rates ⊕ $AWF_PRICING_FILE (whole-entry replace), loaded once.
func Default() Table {
	defaultOnce.Do(func() {
		t, err := loadTable()
		if err != nil {
			panic("pricing: " + err.Error())
		}
		defaultTable = t
	})
	return defaultTable
}

func loadTable() (Table, error) {
	t := Table{}
	if err := json.Unmarshal(embeddedRates, &t); err != nil {
		return nil, fmt.Errorf("embedded rates: %w", err)
	}
	if path := os.Getenv("AWF_PRICING_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("AWF_PRICING_FILE %q: %w", path, err)
		}
		ov := Table{}
		if err := json.Unmarshal(raw, &ov); err != nil {
			return nil, fmt.Errorf("AWF_PRICING_FILE %q: %w", path, err)
		}
		for k, r := range ov {
			if err := validateRates(k, r); err != nil {
				return nil, err
			}
			t[k] = r // whole-entry replace
		}
	}
	return t, nil
}

func validateRates(model string, r Rates) error {
	for _, v := range []float64{r.InputPerM, r.OutputPerM, r.CacheReadPerM, r.CacheWritePerM} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("pricing override %q: rate %v must be finite and non-negative", model, v)
		}
	}
	return nil
}
