// Package ir provides stable IR types (the contract), structural validation, and the definition digest.
package ir

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"
)

// Workflow is the top-level definition. The Digest field is populated by run-start (via
// (*Workflow).SetDigest) or compared on resume; it is excluded from its own hash input
// (`json:"-"`). After json.Unmarshal, Digest is empty until set.
type Workflow struct {
	ID              string                   `json:"workflow"`
	Version         int                      `json:"version"`
	InputSchema     *JSONSchema              `json:"input_schema,omitempty"`
	InputFiles      WorkflowInputFiles       `json:"input_files,omitempty"`
	Env             []string                 `json:"env,omitempty"`
	Assets          map[string]string        `json:"assets,omitempty"`
	Skills          map[string]SkillCorpus   `json:"skills,omitempty"`
	Imports         map[string]string        `json:"imports,omitempty"`
	Agents          map[string]AgentRole     `json:"agents,omitempty"`
	Containers      map[string]Container     `json:"containers"`
	OutputSchema    *JSONSchema              `json:"output_schema,omitempty"`
	Outputs         map[string]TemplateValue `json:"outputs,omitempty"`
	ArtifactExports ArtifactExports          `json:"output_files,omitempty"`
	// Tools is the top-level tools: map (P3 A4) — tool name → definition. Offered
	// to react: steps. Folds into the digest automatically (whole-workflow JCS).
	Tools  map[string]Tool `json:"tools,omitempty"`
	Graph  NodeList        `json:"graph"`
	Digest string          `json:"-"`
}

type SkillCorpus struct {
	From   string `json:"from"`
	Layout string `json:"layout"`
	Router string `json:"router"`
}

type StepSkillRouting struct {
	From  string   `json:"from"`
	Query Template `json:"query"`
	Limit int      `json:"limit"`
	Into  string   `json:"into"`
}

// Container is backed by exactly one of Image or Compose (structural validation per the standard §3).
type Container struct {
	Image     string     `json:"image,omitempty"`
	Compose   string     `json:"compose,omitempty"`
	Service   string     `json:"service,omitempty"`
	Snapshot  string     `json:"snapshot,omitempty"`
	Cmd       []string   `json:"cmd,omitempty"`
	Keepalive *bool      `json:"keepalive,omitempty"`
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
	// Recovery selects how a retry re-runs the step after a transient fault:
	// "continue" resumes the adapter's persistent session, "restart" re-launches
	// fresh. Unset (the omitempty zero value) means "let the engine resolve a
	// per-adapter default" — see engine.effectiveRecovery. omitempty is
	// load-bearing: an unset recovery must marshal to the exact bytes it did
	// before this field existed so ComputeDigest stays byte-identical.
	Recovery string `json:"recovery,omitempty"`
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

// Timeout is a step's timeout policy. On the wire it is EITHER a bare Go-duration
// scalar — the historical wall-clock per-attempt deadline — OR a { wall, idle }
// map. Idle is the maximum time a step may produce no output (no stdout/stderr
// chunk for a code step, no AgentEvent for an agent step) before the attempt is
// cancelled; an idle expiry is a `retryable_failure`, identical in class to a
// wall expiry, and rides the existing retry policy. Only code (run:) and agent
// (uses:) steps carry a *Timeout; await/tool timeouts stay scalar *Duration.
//
// Digest stability: when Idle is nil the value marshals back to the bare
// nanosecond integer a *Duration would emit, so a scalar-form workflow's digest
// is byte-identical to before the map form existed.
type Timeout struct {
	Wall *Duration
	Idle *Duration
}

func (t *Timeout) MarshalJSON() ([]byte, error) {
	// Scalar form: emit exactly what a *Duration field would (digest stability).
	if t.Idle == nil {
		if t.Wall == nil {
			return []byte("null"), nil
		}
		return t.Wall.MarshalJSON()
	}
	// Map form. json.Marshal sorts map keys, and JCS re-sorts at digest time, so
	// ordering here is irrelevant.
	m := make(map[string]*Duration, 2)
	if t.Wall != nil {
		m["wall"] = t.Wall
	}
	m["idle"] = t.Idle
	return json.Marshal(m)
}

func (t *Timeout) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '{' {
		// Map form { wall?, idle? }, strict: an unknown sub-key is rejected here
		// (the AWF1062 unknown-key walker does not descend into `timeout`).
		var m struct {
			Wall *Duration `json:"wall"`
			Idle *Duration `json:"idle"`
		}
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			return err
		}
		t.Wall = m.Wall
		t.Idle = m.Idle
		return nil
	}
	// Scalar form (a quoted Go-duration string, or a bare int in nanoseconds that
	// validateDurationScalars flags as AWF1063) → wall.
	var d Duration
	if err := d.UnmarshalJSON(trimmed); err != nil {
		return err
	}
	t.Wall = &d
	return nil
}

// Template is an unparsed templated string (e.g. "{{ input.cve_id }}:pr"). Parsed in the template package.
type Template string

type TemplateValue = json.RawMessage

type ArtifactExports map[string]string

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
