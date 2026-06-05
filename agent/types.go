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

	// Containerless reports that this adapter performs its work WITHOUT a
	// container (e.g. a direct network call). When true, an agent step may
	// omit `container:` (ir/validate_structural.go allows the empty ref; the
	// run-start guard in cli/runtimes.go permits it only for these adapters).
	// Zero value false → every CLI-wrapping adapter keeps requiring a
	// container, unchanged. The engine does not otherwise branch on Caps
	// (Phase 5 decision 16).
	Containerless bool `json:"containerless"`
}

// SecretEnv is the type used for env-passthrough values that contain secrets
// (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN).
//
// Formatter safety (locked by TestSecretEnv_RedactsInStandardFormatters):
// SecretEnv's String + GoString methods redact values under every fmt verb
// that consults them — `%v`, `%s`, `%q`, `%#v`, and `%+v`. (Even %+v calls
// Stringer on a defined map type; the redaction holds whether SecretEnv is
// printed directly or as a field of AgentInvocation.)
//
// JSON safety: AgentInvocation.Env is tagged `json:"-"` so json.Marshal
// can never serialize the values. The engine's state log is JSON; this
// guarantees env values never reach the journal even if a future caller
// marshals an AgentInvocation.
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
// SECURITY: Env is SecretEnv. Its values redact under all standard fmt
// verbs (locked by TestSecretEnv_RedactsInStandardFormatters), and the
// `json:"-"` tag prevents JSON serialization from ever exposing them.
// See the SecretEnv doc-comment for the formatter + JSON guarantees.
type AgentInvocation struct {
	NodePath       string         `json:"node_path"`                 // engine/path output for the AgentStep (e.g. "graph[0]" or "gate[2].attempt-1.generate[0]")
	Uses           string         `json:"uses"`                      // agent-runtime ref (must match Adapter.Ref())
	With           ir.RawConfig   `json:"with,omitempty"`            // opaque per-runtime config; validated by Adapter.ValidateConfig
	OutputSchema   *ir.JSONSchema `json:"output_schema,omitempty"`   // step's output_schema (the adapter passes to harness as --json-schema or layer-2 extractor schema)
	Env            SecretEnv      `json:"-"`                         // env vars forwarded into the harness exec (slice 5.3 reads ANTHROPIC_API_KEY etc.); never JSON-marshaled so secrets cannot reach the state log
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
	// Display is the adapter-populated, normalized summary the live renderer uses.
	// json:"-" — transient presentation, never journaled (the raw Payload is the
	// durable artifact). Zero value (DisplayOther) → terse fallback.
	Display EventDisplay `json:"-"`
}

// MetricSet aggregates the per-step counters the obs package (Phase 6)
// projects to OTel.
type MetricSet struct {
	Cost   MetricCost   `json:"cost"`
	Tokens MetricTokens `json:"tokens"`
	Turns  int          `json:"turns"`
}

// Agent event Kind values that denote a tool call (Claude emits "tool_use";
// droid emits "tool_call"). Consumers that count tool activity match these.
const (
	AgentEventKindToolUse  = "tool_use"
	AgentEventKindToolCall = "tool_call"
)

// MetricCost.Source values: an adapter that wraps a harness reporting its
// own dollar figure (Claude Code's total_cost_usd) stamps CostSourceReported;
// a future token-only adapter's derived cost (deferred — no pricing package
// ships yet; see runtime-design.md §10) would stamp CostSourceDerived.
const (
	CostSourceReported = "reported"
	CostSourceDerived  = "derived"
)

type MetricCost struct {
	USD    float64 `json:"usd"`
	Source string  `json:"source,omitempty"` // CostSourceReported | CostSourceDerived
}

type MetricTokens struct {
	Input              int `json:"input"`
	Output             int `json:"output"`
	CacheCreationInput int `json:"cache_creation_input,omitempty"`
	CacheReadInput     int `json:"cache_read_input,omitempty"`
}

// AgentOutcome is the envelope the streaming Adapter.Launch contract
// delivers via its outcome channel (slice 5.3 γ). Carries either an
// AgentResult (happy path) or an Err (in-flight failure:
// *ErrUnparseableOutput, *ErrAgentLaunch, *ErrAuthFailureSentinel wrapped
// via *ErrAgentLaunch). Exactly one is populated; the other is the type's
// zero value.
//
// Why an envelope vs (AgentResult, error) tuple? The outcome channel must
// carry both pieces atomically — a struct on a channel is the idiomatic
// way to do that in Go. Tuple-returning a 2-value `(<-chan AgentResult,
// <-chan error)` would force the caller to select between two channels
// with no guaranteed ordering; the envelope avoids that.
type AgentOutcome struct {
	Result AgentResult `json:"result,omitempty"`
	Err    error       `json:"-"` // errors don't JSON-marshal well; engine state log handles errors separately
}
