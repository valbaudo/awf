package engine

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

func appendNodeCompleted(log state.Log, path string, data NodeCompletedData) error {
	if data.Usage == nil {
		// Every node.completed MUST carry usage (cost-first-class spec
		// 2026-08-13): Commit backfills the zero MetricSet for code steps;
		// this guard covers the direct appenders (signal steps, call export
		// products) — absence is a producer bug, the console rejects it.
		data.Usage = &agent.MetricSet{}
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal node.completed for %q: %w", path, err)
	}
	if err := log.Append(state.Event{Type: EventNodeCompleted, Path: path, Data: dataJSON}); err != nil {
		return fmt.Errorf("append node.completed for %q: %w", path, err)
	}
	if err := log.Sync(); err != nil {
		return fmt.Errorf("sync node.completed for %q: %w", path, err)
	}
	return nil
}

// Commit persists a successful node result. The order — Blobs.Put each artifact
// FIRST, then Log.Append, then Log.Sync through appendNodeCompleted — is the
// spec §8 + CLAUDE.md "content-address-then-pointer-swap" invariant in code. A
// crash between Blobs.Put and Log.Append leaves orphan blobs only; no
// node.completed event ever references a missing artifact.
//
// Commit takes the pre-commit DispatchResult and returns the post-commit
// NodeResult (refs filled, original materialized values preserved for callers
// that don't want to round-trip via Blobs.Get).
//
// dr.Outcome MUST be OutcomeOK — non-ok outcomes never commit (their
// node.failed event is the interpreter's responsibility, slice 2.5; Commit
// returns an error to surface the caller bug).
//
// participates is the continues:-threading gate: when true, the adapter-provided
// verbatim {user, assistant} pair (dr.Transcript) is content-addressed alongside
// the other artifacts (before the node.completed append) and recorded as
// TranscriptRef. When false the transcript is ignored entirely (code/signal steps
// and non-conversation agent steps), keeping their logs byte-identical.
//
// Errors at any step propagate. The log is left in a consistent state in all
// failure modes: either the node.completed landed (full success) or it didn't
// (any partial blob writes are orphans, GC-deferred per spec §11.5).
func Commit(log state.Log, blobs state.Blobs, path string, dr DispatchResult, participates bool) (NodeResult, error) {
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
		outBytes, err := marshalCanonicalJSON(dr.Outputs)
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

	// continues: threading — when this step participates in a conversation, the
	// adapter-provided verbatim {user, assistant} pair is content-addressed BEFORE
	// the node.completed append, preserving the Put→Append→Sync ordering. Commit
	// reads NO with: key and never sees `as` — the pair is supplied by the adapter
	// (DispatchResult.Transcript), so with:-opacity is intact.
	var transcriptRef string
	if participates {
		tBytes, err := json.Marshal(dr.Transcript)
		if err != nil {
			return NodeResult{}, fmt.Errorf("engine.Commit: marshal transcript at path %q: %w", path, err)
		}
		ref, err := blobs.Put(tBytes)
		if err != nil {
			return NodeResult{}, fmt.Errorf("engine.Commit: put transcript at path %q: %w", path, err)
		}
		transcriptRef = ref
		nr.Transcript = dr.Transcript
	}

	// session subtree — when the dispatcher captured a claude session projects/
	// subtree (via Backend.ReadTreeAt, a gzip-tar), content-address it before the
	// node.completed append, preserving the Put→Append→Sync ordering (mirrors the
	// transcript block above).
	sessionRef := dr.SessionRef
	if sessionRef == "" && len(dr.SessionTranscript) > 0 {
		ref, err := blobs.Put(dr.SessionTranscript)
		if err != nil {
			return NodeResult{}, fmt.Errorf("engine.Commit: put session transcript: %w", err)
		}
		sessionRef = ref
	}

	// 2. Compute the node_key for deterministic (code) steps.
	// dr.Node is set ONLY on the code-step dispatch path (interpreter.go);
	// all other call sites leave it nil → isDeterministicNode(nil) == false → empty key.
	// runtimePins is nil for v1 (see WS-6b spec: backend already pinned in
	// RunStartedData.Backend; per-image-digest pinning is a follow-up).
	var nodeKey, nodeSubtreeDigest string
	if isDeterministicNode(dr.Node) {
		subtreeDigest, sdErr := ir.NodeSubtreeDigest(dr.Node)
		if sdErr != nil {
			return NodeResult{}, fmt.Errorf("engine.Commit: node subtree digest at path %q: %w", path, sdErr)
		}
		nodeKey = computeNodeKey(subtreeDigest, dr.InputRefs, nil)
		nodeSubtreeDigest = subtreeDigest // reuse the already-computed value; no second call
	}
	nr.NodeKey = nodeKey
	nr.NodeSubtreeDigest = nodeSubtreeDigest

	// 3. Append the node.completed event with all refs. Its existence in the
	// log IS the completion record; the artifacts it references provably
	// already exist (we just Put them).
	data := NodeCompletedData{
		Outcome:    string(dr.Outcome),
		ExitCode:   dr.ExitCode,
		OutputsRef: nr.OutputsRef,
		StdoutRef:  nr.StdoutRef,
		Files:      nr.Files,
		Usage:      dr.Metrics,
		// Slice 7.1 — the snapshot blob was already Put to Blobs by the
		// dispatcher (Backend.Snapshot); Commit records the ref ONLY. No re-Put.
		SnapshotRef:       dr.SnapshotRef,
		Container:         dr.Container,
		TranscriptRef:     transcriptRef,
		NodeKey:           nodeKey,
		NodeSubtreeDigest: nodeSubtreeDigest, // reuses the same subtree value as NodeKey's input; no drift
		SessionRef:        sessionRef,
	}
	if data.Usage == nil {
		// Non-agent step (code/signal): explicit zeros — usage is required on
		// every node.completed (cost-first-class spec 2026-08-13).
		data.Usage = &agent.MetricSet{}
	}
	if err := appendNodeCompleted(log, path, data); err != nil {
		return NodeResult{}, fmt.Errorf("engine.Commit: %w", err)
	}

	return nr, nil
}
