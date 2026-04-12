package engine

import (
	"context"
	"errors"
	"time"

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
}

// ResolvedInputs is what the interpreter precomputes (Phase 2.5) before
// invoking the dispatcher — see the ResolvedInputs contract table in the
// slice 2.4 plan for what each field carries. The dispatcher consumes
// verbatim; no template.Substitute call happens inside.
type ResolvedInputs struct {
	Command               string
	Env                   map[string]string
	OutputFiles           []string
	OutputSchema          *ir.JSONSchema
	NonRetryableExitCodes []int
	Timeout               time.Duration
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
}

// ErrUnsupportedKind is returned by LocalDispatcher for Phase-2-unsupported
// step kinds (AgentStep is Phase 5; SignalStep is Phase 3). Callers can
// errors.Is to route this distinctly from a real dispatch failure — the
// interpreter (slice 2.5) treats it as a hard error halting the run with a
// clear "use this kind in a later phase" message, NOT a retryable failure.
var ErrUnsupportedKind = errors.New("engine: step kind not supported in Phase 2 (agent → Phase 5, signal → Phase 3)")
