package agent

import (
	"fmt"
	"maps"
	"slices"

	"github.com/valbaudo/awf/ir"
)

// Caps reports an Adapter's static capabilities. Read by the conformance
// suite to route adapters to the correct bucket (NativeSchema=true → Bucket
// 14 path; NativeSchema=false → Bucket 15 layer-2 contract). The engine
// itself does NOT branch on Caps (Phase 5 design decision 16) — the
// typed-output contract is on AgentResult.Output regardless of how the
// adapter produced it. Caps is documentation + test routing.
//
// Future fields land here additively as new Phase 5+ adapters surface
// distinguishing capabilities (e.g. SupportsThinking, SupportsTools).
type Caps struct {
	NativeSchema bool `json:"native_schema"`
}

// SecretEnv is the type used for env-passthrough values that contain secrets
// (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN). Its
// fmt.Stringer + fmt.GoStringer methods REDACT the values in `%v`, `%s`,
// `%q`, and `%#v` formatting — preventing accidental leakage via error
// wrapping and log lines.
//
// SECURITY LIMITATION: Go's `%+v` formatter uses reflection to walk fields
// directly and BYPASSES Stringer entirely. The redaction does NOT protect
// against `fmt.Sprintf("%+v", inv)`. Callers MUST NOT use `%+v` on
// AgentInvocation values in any code path that may reach logs, error
// wrapping, or CI output. Use the named fields explicitly:
//
//	fmt.Errorf("launch failed for %s in container %s: %w", inv.Uses, inv.NodePath, err)
//
// NOT:
//
//	fmt.Errorf("launch failed for %+v: %w", inv, err)  // LEAKS the API key
//
// The repo's .golangci.yml may include a forbidigo rule banning `%+v` on
// AgentInvocation; if not, add a follow-up PR.
type SecretEnv map[string]string

// String returns a redacted representation showing key names but not values.
// Called by fmt.Sprintf with %v / %s / %q.
func (e SecretEnv) String() string {
	if len(e) == 0 {
		return "agent.SecretEnv{}"
	}
	// Deterministic key order for test golden output (slices.Sorted + maps.Keys
	// is the Go 1.23+ idiom; consistent with Registry.Refs).
	keys := slices.Sorted(maps.Keys(e))
	return fmt.Sprintf("agent.SecretEnv{keys: %v, values: [REDACTED %d entries]}", keys, len(e))
}

// GoString returns the same redacted representation. Go's %#v formatter
// consults this; without GoString, %#v would print the literal map and
// leak secrets.
func (e SecretEnv) GoString() string { return e.String() }

// AgentInvocation is the per-call input handed to Adapter.Launch. Slice 5.2
// wiring (engine/local_dispatcher.go runAgentStep) constructs one of these
// per AgentStep, resolves the templated With through template.Evaluator,
// reads Env from the run-start env-passthrough, and threads Feedback from
// the gate's prior verdict on repair attempts.
//
// SECURITY: Env is of type SecretEnv whose values are redacted in
// `%v`/`%s`/`%q`/`%#v` formatting. However, `%+v` BYPASSES Stringer and
// will leak the API key value. See SecretEnv doc-comment for the safe
// formatting pattern.
type AgentInvocation struct {
	NodePath       string         `json:"node_path"`                 // engine/path output for the AgentStep (e.g. "graph[0]" or "gate[2].attempt-1.generate[0]")
	Uses           string         `json:"uses"`                      // agent-runtime ref (must match Adapter.Ref())
	With           ir.RawConfig   `json:"with,omitempty"`            // opaque per-runtime config; validated by Adapter.ValidateConfig
	OutputSchema   *ir.JSONSchema `json:"output_schema,omitempty"`   // step's output_schema (the adapter passes to harness as --json-schema or layer-2 extractor schema)
	Env            SecretEnv      `json:"env,omitempty"`             // env vars forwarded into the harness exec (Phase 5 slice 5.3 reads ANTHROPIC_API_KEY etc. from here) — redacted in standard formatters; see SecretEnv doc-comment for the %+v gap
	IdempotencyKey string         `json:"idempotency_key,omitempty"` // resolved-template; passed to harness env per spec §10
	Feedback       ir.RawConfig   `json:"feedback,omitempty"`        // prior gate verdict on repair attempts > 1 (nil on attempt 1)
}

// AgentResult is the synchronous return of Adapter.Launch. Output is the
// validated typed output matching the step's OutputSchema (per spec §4.2 —
// references bind only to typed fields). ExitCode mirrors a process exit
// code shape (0 = ok); the engine reads it but currently only treats
// nonzero as a transport-class signal — quality verdicts live in Output.
// Metrics carries OTel-bound counters (Phase 6 obs reads them). Files maps
// declared output_files paths to their content bytes (Phase 5 adapter
// implementations capture from inside the container; slice 5.2 dispatcher
// Puts these into Blobs at commit boundary).
type AgentResult struct {
	Output   map[string]any    `json:"output,omitempty"`
	ExitCode int               `json:"exit_code"`
	Metrics  MetricSet         `json:"metrics"`
	Files    map[string][]byte `json:"files,omitempty"`
}

// AgentEvent is one slice of progress emitted live on the channel returned
// by Adapter.Launch. Kind is harness-specific (slice 5.3 enumerates the
// stream-json kinds for Claude Code: "system", "assistant", "user",
// "tool_use", "tool_result", "thinking", "result", "rate_limit"). Payload
// is the raw bytes the harness emitted for the event (CAS-offloaded by
// the dispatcher in slice 5.2 if larger than 4 KiB). Stream is "stdout"
// or "stderr" — typed loosely to match container/Backend's IOChunk.
type AgentEvent struct {
	Kind    string `json:"kind"`
	Payload []byte `json:"payload,omitempty"`
	Stream  string `json:"stream,omitempty"`
}

// MetricSet aggregates the per-step counters the obs package (Phase 6)
// projects to OTel. Cost.Source values are "reported" (harness emitted a
// dollar figure — Claude Code's total_cost_usd) or "derived" (Phase 6's
// pricing-table computation; not used in Phase 5).
type MetricSet struct {
	Cost   MetricCost   `json:"cost"`
	Tokens MetricTokens `json:"tokens"`
	Turns  int          `json:"turns"`
}

type MetricCost struct {
	USD    float64 `json:"usd"`
	Source string  `json:"source,omitempty"` // "reported" | "derived"
}

type MetricTokens struct {
	Input              int `json:"input"`
	Output             int `json:"output"`
	CacheCreationInput int `json:"cache_creation_input,omitempty"`
	CacheReadInput     int `json:"cache_read_input,omitempty"`
}
