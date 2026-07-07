package agent

import (
	"fmt"
	"maps"
	"slices"

	"github.com/valbaudo/awf/ir"
)

// Caps reports an Adapter's static capabilities. Read by the conformance
// suite to route adapters to the correct bucket (NativeSchema=true -> Bucket
// 14 path; NativeSchema=false -> Bucket 15 layer-2 contract). The typed-output
// contract remains AgentResult.Output regardless of how the adapter produced
// it; other fields are static declarations used by CLI guards, defensive
// engine guards, conformance routing, and documentation.
//
// Future fields land here additively as new Phase 5+ adapters surface
// distinguishing capabilities (e.g. SupportsThinking, SupportsTools).
type Caps struct {
	NativeSchema bool `json:"native_schema"`

	// Containerless reports that this adapter performs its work WITHOUT a
	// container (e.g. a direct network call). When true, an agent step may
	// omit `container:` (ir/validate_structural.go allows the empty ref; the
	// run-start guard in cli/runtimes.go permits it only for these adapters).
	// Zero value false -> every CLI-wrapping adapter keeps requiring a
	// container, unchanged.
	Containerless bool `json:"containerless"`

	// Threaded reports that this adapter prepends an engine-supplied
	// AgentInvocation.Thread to its request (continues: threading). This drives
	// conformance routing plus CLI and defensive engine guards (a continues:
	// step against a non-Threaded adapter fails fast). Direct Containerless
	// precedent.
	Threaded bool `json:"threaded,omitempty"`

	// ContextEvidence reports that this adapter can render engine-assembled
	// evaluator source context as evidence, without treating it as an active
	// conversation continuation. Normal continues: threading still uses Threaded.
	ContextEvidence bool `json:"context_evidence,omitempty"`

	// PersistentSession reports that this adapter can reuse a live/persistent
	// session across launches. It is a static runtime declaration used by CLI
	// guards, defensive engine guards, conformance routing, and docs. Zero value
	// false preserves the fresh-launch behavior required for gate evaluators;
	// PersistentSession adapters are rejected in gate.evaluate contexts while
	// remaining allowed elsewhere, including gate.generate.
	PersistentSession bool `json:"persistent_session,omitempty"`

	// InlineInputFiles reports that a Containerless adapter delivers a step's
	// input_files to the model as inline message parts (agent.InputFile) rather
	// than staging into a container. awf/llm does this; codex-live does not.
	// Zero value false FAILS CLOSED: the run-start guard rejects a containerless
	// step declaring input_files against a non-inline adapter instead of
	// silently dropping the files.
	InlineInputFiles bool `json:"inline_input_files,omitempty"`

	// IsolatedConfigDir reports that this adapter benefits from a per-run,
	// RunID-keyed isolated config directory (mapped to its config-dir env var,
	// e.g. CLAUDE_CONFIG_DIR) so concurrent runs do not collide on shared host
	// config. The engine computes the dir (<staging-root>/claude-session/<run-id>)
	// and threads it via AgentInvocation.SessionConfigDir; the adapter sets it on
	// the exec env. Container-backed only (a Containerless adapter has no container
	// filesystem to isolate). Orthogonal to PersistentSession, which ADDITIONALLY
	// captures/restores the session as that dir's projects/ subtree.
	IsolatedConfigDir bool `json:"isolated_config_dir,omitempty"`

	// SurfacesLiveness grades how finely this adapter streams live progress
	// signals a stall watchdog can trust as proof the turn is still working.
	// The zero value LivenessNone means "not measured / no signal", so an
	// adapter that hasn't been characterized never overclaims. Only adapters we
	// have MEASURED set a higher tier (codexlive = Coarse, claude-family = Fine).
	SurfacesLiveness Liveness `json:"surfaces_liveness,omitempty"`
}

// Liveness grades the degree to which an Adapter surfaces live progress
// signals (streamed deltas) between the start and end of a turn. A stall
// watchdog reads this to decide how confidently a quiet stretch means "hung"
// rather than "working silently". The zero value is LivenessNone so an
// unmeasured adapter honestly declares no signal instead of overclaiming.
type Liveness uint8

const (
	LivenessNone   Liveness = iota // no live progress signal (default; unmeasured)
	LivenessCoarse                 // coarse progress deltas (e.g. reasoning-summary chunks)
	LivenessFine                   // fine-grained streamed deltas (e.g. thinking_delta tokens)
)

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

// ThreadTurn is one prior (user, assistant) exchange in an engine-owned
// conversation. The engine assembles a slice of these from the durable log
// (continues: threading) and feeds it to the generating turn only; it is
// never a bindable reference and never visible to until/templates.
type ThreadTurn struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}

// RunContext is the explicit run identity passed to every agent invocation.
// CurrentEpoch is the epoch the invocation is executing in; NextEpoch is the
// epoch a live replay request should target. Normal dispatch uses the same
// value for both. Resume preflight can construct requests with distinct values.
type RunContext struct {
	RunID        string `json:"run_id"`
	CurrentEpoch uint32 `json:"current_epoch"`
	NextEpoch    uint32 `json:"next_epoch"`
}

// LiveResumePreflightRequest is the data-only request the CLI gives a live
// adapter before appending run.resumed. The core treats With as opaque after
// template substitution; adapter-owned keys such as session/cwd are validated
// by the adapter's PreflightResume implementation.
type LiveResumePreflightRequest struct {
	NodePath     string       `json:"node_path"`
	AdapterRef   string       `json:"adapter_ref"`
	With         ir.RawConfig `json:"with,omitempty"`
	RunID        string       `json:"run_id"`
	CurrentEpoch uint32       `json:"current_epoch"`
	NextEpoch    uint32       `json:"next_epoch"`
}

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
	NodePath   string       `json:"node_path"`      // engine/path output for the AgentStep (e.g. "graph[0]" or "gate[2].attempt-1.generate[0]")
	Uses       string       `json:"uses"`           // agent-runtime ref (must match Adapter.Ref())
	RunContext RunContext   `json:"run_context"`    // explicit run/epoch identity for live-capable adapters
	With       ir.RawConfig `json:"with,omitempty"` // opaque per-runtime config; validated by Adapter.ValidateConfig
	// RoleWith is the engine-resolved (scope-substituted) role with: for a
	// DerivedAdapter-backed step. The engine substitutes the role's raw templated
	// with: against the step scope and threads it here so the role layer merges as
	// already-rendered config. Nil for a non-role step or a direct caller (tests) —
	// then DerivedAdapter falls back to its stored raw role with:. Engine
	// operational state like Thread/ResumeSession; json:"-": never journaled.
	RoleWith        ir.RawConfig   `json:"-"`
	OutputSchema    *ir.JSONSchema `json:"output_schema,omitempty"`    // step's output_schema (the adapter passes to harness as --json-schema or layer-2 extractor schema)
	Env             SecretEnv      `json:"-"`                          // env vars forwarded into the harness exec (slice 5.3 reads ANTHROPIC_API_KEY etc.); never JSON-marshaled so secrets cannot reach the state log
	IdempotencyKey  string         `json:"idempotency_key,omitempty"`  // resolved-template; passed to harness env per spec §10
	Feedback        ir.RawConfig   `json:"feedback,omitempty"`         // prior gate verdict on repair attempts > 1 (nil on attempt 1)
	Thread          []ThreadTurn   `json:"thread,omitempty"`           // engine-assembled prior turns (separate channel from Feedback); generator-only
	ContextEvidence []ThreadTurn   `json:"context_evidence,omitempty"` // evaluator-only source evidence; not active conversation history
	// InputFiles carries resolved file bytes to a CONTAINERLESS adapter (the
	// containerless analog of container.InputFile staging). Empty for
	// container-backed steps. json:"-": bytes never reach the state log.
	InputFiles []InputFile `json:"-"`
	// Attempt is the 1-based retry-attempt index the engine is currently on for
	// this node (1 on the first try, 2 on the first retry, …). Threaded from
	// NodeIntent.Attempt by the dispatcher so an adapter can distinguish a fresh
	// try from a retry. Engine operational state like ResumeSession, not author
	// with: config; json:"-": never journaled. Zero when a caller builds an
	// AgentInvocation directly (tests) without wiring it.
	Attempt int `json:"-"`
	// RecoveryContinue is true when the resolved retry.recovery strategy for this
	// step is "continue" (resume the persistent session on a retry) rather than
	// "restart". Set by the engine from the merged retry.Policy (engine resolves an
	// unset value to a per-adapter default); meaningful only for a PersistentSession
	// adapter. Combined with Attempt>0 it tells the adapter that a leftover
	// in-progress turn from a prior attempt in THIS run may be abandoned and the
	// thread resumed, instead of hard-halting for a cross-process replay. Engine
	// operational state like Attempt/ResumeSession; json:"-": never journaled.
	RecoveryContinue bool `json:"-"`
	// ResumeSession is set true by the engine when it successfully restored a
	// committed session transcript for this node before calling adapter.Launch.
	// The adapter uses it to select the correct CLI flag:
	//   - false (fresh turn): pass --session-id <uuid> (create/adopt a session)
	//   - true  (restored turn): pass --resume <uuid> (re-prime from restored transcript)
	// It is engine operational state, not author with: config — modelled after
	// the existing Thread/ContextEvidence fields. json:"-": never journaled.
	ResumeSession bool `json:"-"`
	// SessionConfigDir is the absolute per-run CLAUDE_CONFIG_DIR the engine
	// computes for claude-family adapters (<stagingRoot>/claude-session/<run-id>);
	// the adapter forwards it as the CLAUDE_CONFIG_DIR env var so claude relocates
	// its whole config tree per run. Set for any container-backed IsolatedConfigDir
	// adapter — both the base anthropic/claude-code adapter (config isolation only)
	// and the session adapter (which ALSO captures the projects/ subtree). Empty
	// for non-claude / containerless steps.
	// Engine-set, like ResumeSession — keep it out of the journal.
	SessionConfigDir string `json:"-"`
	// WorkflowDir is the absolute directory containing the step's own module's
	// workflow file (ir.LoadedModule.WorkflowPath's directory — the loader
	// resolves imports/assets relative to this same directory). The engine sets
	// it for every agent step (engine/agent_step.go, from ictx.def.Module).
	// Consumed by a Containerless PersistentSession adapter (agent/codexlive,
	// F33) to default a host-side `cwd` when the workflow author omits it —
	// codexlive has no container filesystem, so its `cwd` is a real host path
	// the harness operates in. Engine-set, like SessionConfigDir; empty only
	// when a caller builds an AgentInvocation directly (tests) without wiring it.
	WorkflowDir string `json:"-"`
}

// AgentResult is the synchronous return of Adapter.Launch. Output is the
// validated typed output matching the step's OutputSchema (per spec §4.2 —
// references bind only to typed fields). ExitCode mirrors a process exit
// code shape (0 = ok); the engine reads it but currently only treats
// nonzero as a transport-class signal — quality verdicts live in Output.
// Metrics carries OTel-bound counters (Phase 6 obs reads them). Files contains
// adapter-provided extra artifacts to commit. For container-backed agent steps,
// the engine captures declared output_files from the container at the commit
// boundary; adapters do not need to read those paths themselves. Containerless
// adapters cannot declare output_files.
type AgentResult struct {
	Output   map[string]any    `json:"output,omitempty"`
	ExitCode int               `json:"exit_code"`
	Metrics  MetricSet         `json:"metrics"`
	Files    map[string][]byte `json:"files,omitempty"`
	// Live is a data-only handoff from PersistentSession adapters to the
	// dispatcher/interpreter. It carries operational registry metadata for
	// post-commit finalization; it is never a bindable output.
	Live *LiveDispatch `json:"-"`
	// Transcript is the adapter-provided verbatim {clean user prompt, verbatim
	// final assistant message} pair for continues: threading. The engine reads
	// no with: key — the ADAPTER supplies both halves (reading its own with:
	// legitimately). json:"-": never journaled raw (Phase 4 Commit content-
	// addresses it as a blob ref when the step participates in a conversation),
	// matching the Env SecretEnv json:"-" precedent.
	Transcript ThreadTurn `json:"-"`
}

// LiveDispatch is the adapter-owned, data-only metadata needed to reconcile a
// successful persistent turn after node.completed has been committed. The raw
// SessionKey is in-memory only so the registry path can be updated; durable AWF
// events should use SessionKeyHash if they ever need to reference the session.
type LiveDispatch struct {
	AdapterRef     string
	SessionKey     string
	SessionKeyHash string
	LeaseID        string
	ActiveTurnID   string
	ProviderTurnID string
	RunID          string
	NodePath       string
	Epoch          uint32
	CommittedUnix  int64
}

// AgentEvent is one slice of progress emitted live on the channel returned
// by Adapter.Launch. Kind is harness-specific (slice 5.3 enumerates the
// stream-json kinds for Claude Code: "system", "assistant", "user",
// "tool_use", "tool_result", "thinking", "result", "rate_limit"). Payload
// is the raw bytes a strict adapter's harness emitted for the event
// (CAS-offloaded by the dispatcher in slice 5.2 if larger than 4 KiB).
// Live adapters set Live and must put only normalized/redacted durable payload
// bytes here — never raw provider transcripts. Stream is "stdout" or "stderr"
// — typed loosely to match container/Backend's IOChunk.
type AgentEvent struct {
	Kind    string `json:"kind"`
	Payload []byte `json:"payload,omitempty"`
	Stream  string `json:"stream,omitempty"`
	Live    bool   `json:"live,omitempty"`
	// Display is the adapter-populated, normalized summary the live renderer uses.
	// json:"-" — transient presentation. For Live events, the interpreter copies
	// sanitized scalar display metadata into agent.event; it never serializes this
	// struct directly. Zero value (DisplayOther) → terse fallback.
	Display EventDisplay `json:"-"`
}

// MetricSet aggregates the per-step counters the obs package (Phase 6)
// projects to OTel.
type MetricSet struct {
	Cost   MetricCost   `json:"cost"`
	Tokens MetricTokens `json:"tokens"`
	Turns  int          `json:"turns"`
	// Model is the id pricing keyed on: the resolved model if the harness reported it
	// (claude system/init, codexlive thread/start), else the requested with:{model}, else empty.
	Model string `json:"model,omitempty"`
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

// MetricCost is a step's cost. Two states discriminated by Source:
//
//	reported = harness gave a per-call total (Claude total_cost_usd): Total set, NO split.
//	derived  = computed from rates: INVARIANT Total == Input + Output.
type MetricCost struct {
	Source   string  `json:"source,omitempty"`   // CostSourceReported | CostSourceDerived
	Currency string  `json:"currency,omitempty"` // ISO-4217; empty == USD
	Total    float64 `json:"total,omitempty"`
	Input    float64 `json:"input,omitempty"`  // derived only; includes cache, folded (future task)
	Output   float64 `json:"output,omitempty"` // derived only
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
