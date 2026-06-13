// Package pricing converts normalized token counts into a per-model USD cost.
// It is the SOLE constructor of a derived agent cost: Total is always Input+Output.
package pricing

import (
	"regexp"
	"strings"
)

type Rates struct {
	Currency       string  `json:"currency"`
	InputPerM      float64 `json:"input_per_m"`
	OutputPerM     float64 `json:"output_per_m"`
	CacheReadPerM  float64 `json:"cache_read_per_m"`
	CacheWritePerM float64 `json:"cache_write_per_m"`
	UpdatedOn      string  `json:"updated_on,omitempty"`
	Source         string  `json:"source,omitempty"`
}

type Table map[string]Rates

// Breakdown is NORMALIZED token counts: Input EXCLUDES cached.
type Breakdown struct{ Input, Output, CacheRead, CacheWrite int }

type Cost struct {
	Currency             string
	Input, Output, Total float64
}

var (
	reProvider = regexp.MustCompile(`^(anthropic|openai|azure_ai|bedrock|vertex_ai)/`)
	reRegion   = regexp.MustCompile(`^(us|eu|apac)\.anthropic\.`)
	reNS       = regexp.MustCompile(`^anthropic\.`)
	reBedrock  = regexp.MustCompile(`-v\d+(:\d+)?$`)
	reDate     = regexp.MustCompile(`([-@]\d{8})$`)
)

func candidates(model string) []string {
	out := []string{model}
	m := model
	for reProvider.MatchString(m) {
		m = reProvider.ReplaceAllString(m, "")
		out = append(out, m)
	}
	if reRegion.MatchString(m) {
		m = reNS.ReplaceAllString(reRegion.ReplaceAllString(m, "anthropic."), "")
		out = append(out, m)
	}
	if reNS.MatchString(m) {
		m = reNS.ReplaceAllString(m, "")
		out = append(out, m)
	}
	if reBedrock.MatchString(m) {
		m = reBedrock.ReplaceAllString(m, "")
		out = append(out, m)
	}
	if reDate.MatchString(m) {
		out = append(out, reDate.ReplaceAllString(m, ""))
	}
	return out
}

// Derive prices a normalized Breakdown. ok=false on an unknown model.
// Total is set here as Input+Output, exactly once.
func (t Table) Derive(model string, b Breakdown) (Cost, bool) {
	if t == nil || strings.TrimSpace(model) == "" {
		return Cost{}, false
	}
	for _, key := range candidates(model) {
		r, ok := t[key]
		if !ok {
			continue
		}
		in := perM(b.Input, r.InputPerM) + perM(b.CacheRead, r.CacheReadPerM) + perM(b.CacheWrite, r.CacheWritePerM)
		outc := perM(b.Output, r.OutputPerM)
		return Cost{Currency: r.Currency, Input: in, Output: outc, Total: in + outc}, true
	}
	return Cost{}, false
}

func perM(tokens int, ratePerM float64) float64 { return float64(tokens) / 1_000_000 * ratePerM }
