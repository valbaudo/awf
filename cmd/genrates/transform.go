package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/valbaudo/awf/pricing"
)

// sourceURL / liteLLMSourceURL are recorded in each generated entry's `source`
// field so the provenance of a value is honest: the rate came from the
// aggregator, not from a human reading the provider's own pricing page.
const (
	sourceURL        = "https://models.dev"
	liteLLMSourceURL = "https://github.com/BerriAI/litellm"
)

// modelsDevDoc is the SUBSET of models.dev api.json we read: provider -> models -> entry.
// All other top-level and per-model fields are ignored.
type modelsDevDoc map[string]struct {
	Models map[string]modelsDevEntry `json:"models"`
}

type modelsDevEntry struct {
	Cost        *modelsDevCost `json:"cost"`
	LastUpdated string         `json:"last_updated"`
}

// modelsDevCost reads only the BASE (top-of-object) per-million rates. The
// sibling `tiers`, `context_over_200k`, `input_audio`, etc. are intentionally
// NOT read — AWF's Rates is a flat 4-field model and prices the base tier
// (documented estimate caveat for long-context/tiered models).
type modelsDevCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

func parseModelsDev(raw []byte) (modelsDevDoc, error) {
	var doc modelsDevDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse models.dev: %w", err)
	}
	return doc, nil
}

// lookupModelsDev finds id in the FIRST first-party provider (in firstParty
// order) that carries it with a non-nil cost, returning the transformed Rates.
// Searching only first-party providers (openai/anthropic/google/...) avoids the
// marked-up prices an aggregator/reseller entry might carry for the same id.
// ok=false (no error) if id is in none of them; a present-but-invalid cost is a
// hard error (fail-closed).
func lookupModelsDev(doc modelsDevDoc, firstParty []string, id string) (pricing.Rates, bool, error) {
	for _, p := range firstParty {
		pe, ok := doc[p]
		if !ok {
			continue
		}
		me, ok := pe.Models[id]
		if !ok || me.Cost == nil {
			continue
		}
		r := pricing.Rates{
			Currency:       "USD",
			InputPerM:      me.Cost.Input, // models.dev is already per-million
			OutputPerM:     me.Cost.Output,
			CacheReadPerM:  me.Cost.CacheRead,
			CacheWritePerM: me.Cost.CacheWrite,
			UpdatedOn:      me.LastUpdated,
			Source:         sourceURL,
		}
		if err := validateRates(id, r); err != nil {
			return pricing.Rates{}, false, err
		}
		return r, true, nil
	}
	return pricing.Rates{}, false, nil
}

// liteLLMEntry is the subset of a LiteLLM model_prices_and_context_window.json
// entry we read. Values are PER-TOKEN (USD) and are scaled to per-million here.
type liteLLMEntry struct {
	InputCostPerToken           *float64 `json:"input_cost_per_token"`
	OutputCostPerToken          *float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost     *float64 `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost *float64 `json:"cache_creation_input_token_cost"`
}

// lookupLiteLLM is the FALLBACK source: it resolves id from LiteLLM's flat,
// per-token map (multiplying by 1e6). LiteLLM has no per-model date, so the
// passed sync date is stamped. ok=false if id is absent or lacks input/output.
func lookupLiteLLM(raw []byte, id, syncDate string) (pricing.Rates, bool, error) {
	var m map[string]liteLLMEntry
	if err := json.Unmarshal(raw, &m); err != nil {
		return pricing.Rates{}, false, fmt.Errorf("parse litellm: %w", err)
	}
	e, ok := m[id]
	if !ok || e.InputCostPerToken == nil || e.OutputCostPerToken == nil {
		return pricing.Rates{}, false, nil
	}
	r := pricing.Rates{
		Currency:   "USD",
		InputPerM:  *e.InputCostPerToken * 1e6,
		OutputPerM: *e.OutputCostPerToken * 1e6,
		UpdatedOn:  syncDate,
		Source:     liteLLMSourceURL,
	}
	if e.CacheReadInputTokenCost != nil {
		r.CacheReadPerM = *e.CacheReadInputTokenCost * 1e6
	}
	if e.CacheCreationInputTokenCost != nil {
		r.CacheWritePerM = *e.CacheCreationInputTokenCost * 1e6
	}
	if err := validateRates(id, r); err != nil {
		return pricing.Rates{}, false, err
	}
	return r, true, nil
}

// validateRates fails closed: input/output MUST be positive (a real chat model
// always has both; a 0 means a free/placeholder/embedding entry slipped in and a
// $0 derived cost would be worse than 'absent'), and every rate must be finite
// and non-negative. Mirrors pricing.validateRates' finite/non-negative intent.
func validateRates(id string, r pricing.Rates) error {
	if r.InputPerM <= 0 || r.OutputPerM <= 0 {
		return fmt.Errorf("%s: input/output rate must be positive (got input=%v output=%v)", id, r.InputPerM, r.OutputPerM)
	}
	for _, v := range []float64{r.InputPerM, r.OutputPerM, r.CacheReadPerM, r.CacheWritePerM} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("%s: rate %v must be finite and non-negative", id, v)
		}
	}
	return nil
}

// build resolves every allowlisted id into a pricing.Table: models.dev first,
// then a LAZY LiteLLM fallback (fetchLiteLLM is called at most once, only if
// something is missing). Any id absent from BOTH sources is a hard error
// (fail-closed) — a silently dropped model would read as "unknown ⇒ absent" at
// runtime, masking a stale allowlist or an upstream rename. fetchLiteLLM may be
// nil to disable the fallback (tests / offline).
func build(modelsDevRaw []byte, allowlist, firstParty []string, syncDate string, fetchLiteLLM func() ([]byte, error)) (pricing.Table, error) {
	doc, err := parseModelsDev(modelsDevRaw)
	if err != nil {
		return nil, err
	}
	table := pricing.Table{}
	var misses []string
	var liteLLMRaw []byte
	var liteLLMFetched bool
	for _, id := range allowlist {
		r, ok, err := lookupModelsDev(doc, firstParty, id)
		if err != nil {
			return nil, err
		}
		if ok {
			table[id] = r
			continue
		}
		if fetchLiteLLM != nil {
			if !liteLLMFetched {
				liteLLMRaw, err = fetchLiteLLM()
				if err != nil {
					return nil, fmt.Errorf("litellm fallback fetch: %w", err)
				}
				liteLLMFetched = true
			}
			r, ok, err = lookupLiteLLM(liteLLMRaw, id, syncDate)
			if err != nil {
				return nil, err
			}
			if ok {
				table[id] = r
				continue
			}
		}
		misses = append(misses, id)
	}
	if len(misses) > 0 {
		sort.Strings(misses)
		return nil, fmt.Errorf("%d allowlisted model(s) not found in models.dev (first-party providers %v) or the litellm fallback: %v", len(misses), firstParty, misses)
	}
	return table, nil
}

// render serializes the table as stable, sorted, 2-space-indented JSON with a
// trailing newline — matching pricing/rates.json's committed format so an
// unchanged upstream yields an EMPTY diff (a no-op regen PR). encoding/json
// sorts map keys and emits struct fields in declaration order.
func render(table pricing.Table) ([]byte, error) {
	out, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
