// Package ir provides stable IR types (the contract), structural validation, and the definition digest.
package ir

import (
	"encoding/json"
	"strconv"
	"time"
)

// Workflow is the top-level definition. The Digest field is populated by run-start (via
// (*Workflow).SetDigest) or compared on resume; it is excluded from its own hash input
// (`json:"-"`). After json.Unmarshal, Digest is empty until set.
type Workflow struct {
	ID         string               `json:"workflow"`
	Version    int                  `json:"version"`
	Input      *JSONSchema          `json:"input,omitempty"`
	Containers map[string]Container `json:"containers"`
	Graph      NodeList             `json:"graph"`
	Digest     string               `json:"-"`
}

// Container is backed by exactly one of Image or Compose (structural validation per the standard §3).
type Container struct {
	Image     string     `json:"image,omitempty"`
	Compose   string     `json:"compose,omitempty"`
	Service   string     `json:"service,omitempty"`
	Snapshot  string     `json:"snapshot,omitempty"`
	Resources *Resources `json:"resources,omitempty"`
}

type Resources struct {
	CPU string `json:"cpu,omitempty"`
	Mem string `json:"mem,omitempty"`
}

// RetryPolicy mirrors the spec §6 default shape.
type RetryPolicy struct {
	Attempts              int       `json:"attempts,omitempty"`
	Backoff               string    `json:"backoff,omitempty"`
	Initial               *Duration `json:"initial,omitempty"`
	Max                   *Duration `json:"max,omitempty"`
	NonRetryableExitCodes []int     `json:"non_retryable_exit_codes,omitempty"`
}

// Duration marshals to integer nanoseconds (digest stability); parses int ns or a Go duration string.
type Duration time.Duration

func (d *Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(*d), 10)), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = Duration(parsed)
		return nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return err
	}
	*d = Duration(n)
	return nil
}

// Template is an unparsed templated string (e.g. "{{ input.cve_id }}:pr"). Parsed in the template package.
type Template string

// Expr is an unparsed condition expression (if.cond / loop.until / gate.until).
type Expr string

// RawConfig is opaque agent config (spec §4.2). The core never reads its keys; the adapter validates it.
type RawConfig map[string]any

// Ratio is map.min_success: a count (e.g. 3) or a fraction (e.g. 0.8). It is a type ALIAS to
// json.Number, not a defined type — a defined type over json.Number loses the special numeric-token
// decoding (verified during planning), so `min_success: 3` would fail to unmarshal.
//
// Consumption (engine `map` fan-in, runtime-design.md §5): call .Int64() or .Float64() to
// interpret; the consumer must check which form. Strictness (rejecting a JSON-string form like
// "0.8" that json.Number also accepts) is the validator's job — see runtime-design.md §4.
type Ratio = json.Number

// JSONSchema is a JSON Schema document, preserved as decoded JSON for canonicalization + validation.
// A defined map type is sufficient — json.Marshal sorts map keys alphabetically (digest-stable),
// and json.Unmarshal decodes any object directly. No custom marshalers needed (verified).
type JSONSchema map[string]any
