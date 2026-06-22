package engine

import (
	"context"
	"errors"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
)

// Dispatcher is the seam between the interpreter (what to run) and the executor
// (how to run it). Phase 2 ships one concrete impl (LocalDispatcher, in
// local_dispatcher.go); future impls (QueueDispatcher, K8sDispatcher) slot in
// without an interpreter rewrite per runtime-design.md §5.
//
// The Dispatcher executes ONE attempt of a single node and returns its raw
// (pre-commit) result. Retry orchestration lives in engine.RunWithRetry; the
// commit boundary lives in engine.Commit. The dispatcher itself does NOT
// touch state.Log or state.Blobs (CLAUDE.md invariant: "the interpreter is
// the only writer to state" — and the dispatcher is downstream of the
// interpreter, not the interpreter itself).
//
// Returned channel: the per-attempt IOChunk stream from container.Backend.Exec.
// The caller (RunWithRetry / interpreter) drains it for the live tap; the
// channel is closed by the backend before Exec returns (matches the slice 2.2
// fake's pre-buffered shape; the Docker impl in Phase 4 may stream live —
// receive-only direction keeps both compatible). On non-nil error, the channel
// is nil; callers MUST nil-check before ranging (a `for range nil-chan`
// deadlocks).
type Dispatcher interface {
	Run(ctx context.Context, intent NodeIntent) (DispatchResult, <-chan container.IOChunk, error)
}

// NodeIntent carries everything one dispatch attempt needs. The interpreter
// (slice 2.5) builds one per code-step (Phase 2 has no per-attempt template
// refs, so the same intent is passed to every attempt of RunWithRetry).
//
// IdempotencyKey is the template.Substitute'd value of CodeStep.IdempotencyKey
// (empty if the step has no idempotency_key declared). The dispatcher exposes
// it to the step via the AWF_IDEMPOTENCY_KEY env var. AWF §10: "stable template
// the external system uses to dedupe; the runtime passes the resolved key to
// the step on every attempt." Phase 2 is the once-substituted form per plan
// Design question 2 + ResolvedInputs contract.
type NodeIntent struct {
	Path           string
	Node           ir.Node
	ResolvedInputs ResolvedInputs
	IdempotencyKey string
	IsGateEvaluate bool
	RunContext     agent.RunContext

	agentEventSink *agentEventSink
}

// ResolvedInputs is what the interpreter precomputes (Phase 2.5) before
// invoking the dispatcher — see the ResolvedInputs contract table in the
// slice 2.4 plan for what each field carries. The dispatcher consumes
// verbatim; no template.Substitute call happens inside.
type ResolvedInputs struct {
	Command               string
	Env                   map[string]string
	OutputFiles           []string
	OutputFileContracts   map[string]OutputFileContract
	OutputSchema          *ir.JSONSchema
	NonRetryableExitCodes []int
	Timeout               time.Duration

	// InputFiles are the resolved (path → bytes) artifacts to stage via
	// Backend.CopyTo BEFORE Exec/Launch. The interpreter resolves each ref to a
	// CAS blob and Blobs.Get's the bytes (the dispatcher never touches Blobs).
	InputFiles []container.InputFile

	// ContainerlessFiles are the resolved input_files for a CONTAINERLESS agent
	// step — delivered to the adapter as inline message parts (each a label +
	// content + sniffed MIME) instead of staged into a container. Populated by
	// engine/agent_step.go's runAgentStep only when the step omits container:;
	// nil for container-backed steps (which use InputFiles) and code steps.
	ContainerlessFiles []agent.InputFile

	// Snapshot is "workspace" iff the step's container is a snapshot:workspace
	// container; the dispatcher captures a CoW diff after a successful exec.
	Snapshot string

	// Slice 5.2 — agent-step fields. Zero values when the node is a CodeStep
	// (runCode ignores them); populated by engine/agent_step.go runAgentStep
	// before dispatch.
	Uses string       // matches AgentStep.Uses; LocalDispatcher.runAgent looks up resolver.Lookup(Uses)
	With ir.RawConfig // post-template-substitution opaque adapter config; adapter.ValidateConfig + adapter.Launch read it

	// Slice 5.3 — agent-step gate feedback (the prior evaluator verdict on
	// repair attempts N>1). Populated by engine/agent_step.go runAgentStep
	// from the enclosing gate's runstate.LookupGateAttempts(gatePath) when
	// the step's path is inside a `.generate.` subtree. Nil on attempt 1 of
	// a gate, on non-gate paths, and for non-agent steps.
	//
	// The dispatcher's runAgent threads this into AgentInvocation.Feedback,
	// where adapters that consume an implicit "previous verdict:" preamble
	// (the Claude Code adapter, slice 5.3) read it. This is the SECONDARY
	// channel for gate feedback; the PRIMARY channel is the author-controlled
	// template substitution `{{ evaluate.<field> }}` that lands in With.prompt
	// before the dispatcher runs (Phase 3.3 wiring, unchanged).
	Feedback ir.RawConfig

	// Thread mirrors Feedback's plumbing: assembled by engine/agent_step.go from the
	// committed log via stepRuntimePath, copied into AgentInvocation.Thread by runAgent.
	// The dispatcher has no RunState access, so assembly happens interpreter-side.
	Thread []agent.ThreadTurn

	// ContextEvidence carries evaluator-only source evidence assembled by the
	// interpreter. It is copied into AgentInvocation.ContextEvidence by runAgent
	// and must not be rendered as active prior conversation.
	ContextEvidence []agent.ThreadTurn
}

type OutputFileContract struct {
	Format string
	Schema *ir.JSONSchema
}

// LiveDispatchRecord is the data-only handoff a live-capable dispatcher returns
// after a successful turn. The interpreter may finalize it after node.completed
// is committed and synced. It intentionally contains no callbacks or hidden
// behavior; ownership of finalization hooks stays in RunOptions.
type LiveDispatchRecord struct {
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

// DispatchResult is the pre-commit shape returned by Dispatcher.Run. The
// interpreter (slice 2.5) passes it to engine.Commit to (a) Put each artifact
// to Blobs and (b) append a node.completed event — Commit then returns the
// post-commit NodeResult that lands in RunState.Completed.
//
// Outcome is mechanically classified per spec §6 (ClassifyOutcome). On a
// non-ok outcome, Err carries the underlying cause (the AWFOutput parse
// error, the Backend.Exec transport error, ctx.DeadlineExceeded for timeout)
// — engine.RunWithRetry surfaces it as the final returned error if retries
// are exhausted; the interpreter (slice 2.5) reads it to attribute a
// node.failed event.
type DispatchResult struct {
	Outcome  Outcome
	ExitCode *int
	Outputs  map[string]any           // pre-commit typed outputs (nil if step has no output_schema or parse failed)
	Stdout   []byte                   // pre-commit raw stdout
	Files    []container.CapturedFile // pre-commit captured file contents (path + bytes)
	Err      error                    // set on non-ok outcomes; nil on Outcome == ok

	// RetryAfter is an optional server-supplied "wait this long before retrying"
	// hint surfaced on a retryable_failure — parsed by an adapter from a provider
	// Retry-After header or a rate-limit reset time (e.g. Anthropic
	// anthropic-ratelimit-*-reset, or the Claude Code rate_limit event's resetsAt).
	// Zero means "no hint, use the policy curve". RunWithRetry feeds it to
	// retry.Policy.EffectiveBackoff so the next sleep waits at least this long
	// (capped at retry.MaxHonoredRetryAfter). Runtime-only: never journaled —
	// Commit reads named fields off DispatchResult, not this one.
	RetryAfter time.Duration

	// Slice 5.2 — agent-step events. The dispatcher's runAgent drains the
	// adapter's <-chan AgentEvent and buffers each here for the
	// interpreter-level engine/agent_step.go to write as agent.event log
	// entries (Blobs-offload ≥ 4 KiB). Nil for code steps. Per CLAUDE.md
	// "interpreter is the only writer to state" — the dispatcher does NOT
	// call Log.Append; it buffers and returns.
	AgentEvents []agent.AgentEvent

	// Slice 6.1 — the adapter's per-step metrics (cost/tokens/turns), produced
	// by runAgent from agent.AgentResult.Metrics. nil for code/signal steps.
	// Commit persists it VERBATIM on node.completed; the engine never
	// interprets it. obs (Phase 6) projects it into awf.cost.* / gen_ai.usage.*.
	Metrics *agent.MetricSet

	// Transcript is the adapter-provided verbatim {user, assistant} pair. Commit
	// content-addresses it when the step participates in a conversation. The engine
	// never holds a with:-derived prompt — closes the opacity gap.
	Transcript agent.ThreadTurn

	// Live is an optional data-only live dispatch handoff. If present, the
	// interpreter calls RunOptions.LiveFinalizer after node.completed commits.
	Live *LiveDispatchRecord

	// SnapshotRef is the CoW workspace diff captured by the dispatcher after a
	// successful exec, for a snapshot:workspace container (empty otherwise).
	// The Backend already Put it to Blobs; Commit records it, never re-Puts.
	SnapshotRef string
	// Container is the step's bare container name (for node.completed.container —
	// resume's snapshot→container mapping + obs's awf.container.name).
	Container string

	// Node and InputRefs are populated ONLY on the code-step dispatch path
	// (engine/interpreter.go) so Commit can compute the NodeKey for deterministic
	// nodes. All other Commit call sites (agent_step, react, reduce) leave these
	// zero (nil Node → isDeterministicNode returns false → empty key). Do NOT
	// populate these from non-code paths.
	Node      ir.Node  // the ir.CodeStep node; nil for all non-code paths
	InputRefs []string // sorted CAS refs of the resolved input_files blobs; nil if no input_files
}

// ErrUnsupportedKind is returned by LocalDispatcher for kinds it doesn't
// directly handle in this slice. Phase 5 slice 5.2 wires AgentStep through
// LocalDispatcher.runAgent; SignalStep is handled by the interpreter
// (engine/signal_step.go) and never reaches the dispatcher (its case in
// LocalDispatcher.Run is defense-in-depth only).
var ErrUnsupportedKind = errors.New("engine: step kind not supported by LocalDispatcher (signal steps go through engine/signal_step.go)")
