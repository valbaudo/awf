package engine

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/state"
)

// Commit is the only function in the engine that appends a node.completed
// event. The order — Blobs.Put each artifact FIRST, then Log.Append, then
// Log.Sync — is the spec §8 + CLAUDE.md "content-address-then-pointer-swap"
// invariant in code. A crash between Blobs.Put and Log.Append leaves orphan
// blobs only; no node.completed event ever references a missing artifact.
//
// Commit takes the pre-commit DispatchResult and returns the post-commit
// NodeResult (refs filled, original materialized values preserved for callers
// that don't want to round-trip via Blobs.Get).
//
// dr.Outcome MUST be OutcomeOK — non-ok outcomes never commit (their
// node.failed event is the interpreter's responsibility, slice 2.5; Commit
// returns an error to surface the caller bug).
//
// Errors at any step propagate. The log is left in a consistent state in all
// failure modes: either the node.completed landed (full success) or it didn't
// (any partial blob writes are orphans, GC-deferred per spec §11.5).
func Commit(log state.Log, blobs state.Blobs, path string, dr DispatchResult) (NodeResult, error) {
	if dr.Outcome != OutcomeOK {
		return NodeResult{}, fmt.Errorf("engine.Commit: refusing to commit non-ok outcome %q at path %q (only ok-steps commit per spec §8)", dr.Outcome, path)
	}

	// 1. Put each artifact to Blobs. Order: outputs first (typed, most likely
	// to be referenced downstream), then stdout, then files. Each Put is
	// idempotent (same content → same ref), so a partial-then-resumed commit
	// produces the same refs.
	nr := NodeResult{
		Outcome:  dr.Outcome,
		ExitCode: dr.ExitCode,
		Outputs:  dr.Outputs,
		Stdout:   dr.Stdout,
	}

	if dr.Outputs != nil {
		outBytes, err := json.Marshal(dr.Outputs)
		if err != nil {
			return NodeResult{}, fmt.Errorf("engine.Commit: marshal outputs at path %q: %w", path, err)
		}
		ref, err := blobs.Put(outBytes)
		if err != nil {
			return NodeResult{}, fmt.Errorf("engine.Commit: put outputs at path %q: %w", path, err)
		}
		nr.OutputsRef = ref
	}

	if len(dr.Stdout) > 0 {
		ref, err := blobs.Put(dr.Stdout)
		if err != nil {
			return NodeResult{}, fmt.Errorf("engine.Commit: put stdout at path %q: %w", path, err)
		}
		nr.StdoutRef = ref
	}

	if len(dr.Files) > 0 {
		nr.Files = make(map[string]string, len(dr.Files))
		for _, f := range dr.Files {
			ref, err := blobs.Put(f.Content)
			if err != nil {
				return NodeResult{}, fmt.Errorf("engine.Commit: put file %q at path %q: %w", f.Path, path, err)
			}
			nr.Files[f.Path] = ref
		}
	}

	// 2. Append the node.completed event with all refs. Its existence in the
	// log IS the completion record; the artifacts it references provably
	// already exist (we just Put them).
	data := NodeCompletedData{
		Outcome:    string(dr.Outcome),
		ExitCode:   dr.ExitCode,
		OutputsRef: nr.OutputsRef,
		StdoutRef:  nr.StdoutRef,
		Files:      nr.Files,
		Metrics:    dr.Metrics,
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return NodeResult{}, fmt.Errorf("engine.Commit: marshal node.completed at path %q: %w", path, err)
	}
	if err := log.Append(state.Event{
		Type: EventNodeCompleted,
		Path: path,
		Data: dataJSON,
	}); err != nil {
		return NodeResult{}, fmt.Errorf("engine.Commit: append node.completed at path %q: %w", path, err)
	}

	// 3. fsync. node.completed is the spec §8 durability-critical event;
	// retry.attempt and the rest ride the next fsync.
	if err := log.Sync(); err != nil {
		return NodeResult{}, fmt.Errorf("engine.Commit: sync log after node.completed at path %q: %w", path, err)
	}

	return nr, nil
}
