package engine

// Phase 2.1 event-type names — the events the fold dispatches on. These are the
// wire-format string values stored in state.Event.Type; renaming any of them would
// invalidate every existing log. The vocabulary expands as later slices add writers:
// 2.4 introduces "retry.attempt"; 2.5 introduces "node.started" / "node.failed" /
// "run.finished"; future phases add "signal.received" / "map.item" / "agent.event" /
// "io.chunk" / …. The fold's default switch arm ignores anything unknown — obs
// (Phase 6) projects them via its own dispatch.
const (
	EventRunStarted    = "run.started"
	EventRunResumed    = "run.resumed"
	EventNodeCompleted = "node.completed"
	EventBranchTaken   = "branch.taken"
	EventLoopIter      = "loop.iter"
)

// RunStartedData is the payload of the first event in a run (and the only event the
// run.id, workflow_digest, and input_ref live in — the fold reads them from here).
//
// Phase 2: Runtimes is always empty (no `uses:` execution). Phase 5 populates it
// with {ref, resolved-version} per agent step, and resume verifies the resolved
// versions against the live registry. `omitempty` on Runtimes means both nil and
// empty-slice writers produce identical on-disk JSON (the key is absent) — avoids
// the silent `"runtimes":null` vs `"runtimes":[]` wire drift a Phase 5 writer would
// otherwise create by forgetting to initialize an empty slice.
type RunStartedData struct {
	RunID          string            `json:"run_id"`
	WorkflowDigest string            `json:"workflow_digest"`
	InputRef       string            `json:"input_ref,omitempty"` // empty if Workflow.Input is nil
	Runtimes       []ResolvedRuntime `json:"runtimes,omitempty"`
}

// ResolvedRuntime is one element of RunStartedData.Runtimes — a `uses:` ref + the
// concrete version it resolved to. Phase 2 doesn't populate this; the type exists
// so the wire format is stable from Phase 5 (when agent adapters land).
type ResolvedRuntime struct {
	Ref     string `json:"ref"`
	Version string `json:"version"`
}

// RunResumedData is the payload of the run.resumed event — emitted at the top of every
// `awf resume` by slice 2.6. The Epoch counter is the resume-invocation index (1 after
// the first `awf run`; 2 after the first `awf resume`; …).
type RunResumedData struct {
	Epoch uint32 `json:"epoch"`
}

// NodeCompletedData is the commit-class event: the engine appends this only after the
// step's typed outputs and any declared output_files have been Put into Blobs. Its
// existence in the log IS the completion record (spec §8). On resume the fold reads
// these to skip already-committed nodes.
//
// OutputsRef is empty if the step has no output_schema (no typed outputs); Files is
// empty if the step has no output_files. omitempty keeps the on-disk JSON minimal.
type NodeCompletedData struct {
	Outcome    string            `json:"outcome"` // always "ok" — only ok-steps commit
	ExitCode   *int              `json:"exit_code,omitempty"`
	OutputsRef string            `json:"outputs_ref,omitempty"`
	Files      map[string]string `json:"files,omitempty"` // declared path → CAS ref
}

// BranchTakenData is the if-decision marker (spec §5.1). Fold uses Which to know which
// branch was visited so resume re-walks the same one. Always "then" or "else".
type BranchTakenData struct {
	Which string `json:"which"` // "then" | "else"
}

// LoopIterData is the per-iteration marker emitted at the end of each loop body (spec
// §5.2 — do-while, so this fires AFTER the iteration's body completes). Fold tracks
// the max N per loop path so resume knows how many iterations have committed.
type LoopIterData struct {
	N int `json:"n"` // 1-based iteration number
}
