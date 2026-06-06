package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/gowebpki/jcs"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/state"
)

// reduceManifestPath is where the canonical-JSON aggregate of branch typed
// outputs is staged in a run: reducer's container (spec §3.2a).
const reduceManifestPath = "/work/.awf/aggregate.json"

// reduceBranch is one committed branch's contribution: its typed outputs + its
// named artifacts (declared container path → CAS ref), index-ordered by the
// caller (engine/map.go's collectReduceBranches, Task 11).
type reduceBranch struct {
	N       int
	Outputs map[string]any
	Files   map[string]string // declared container path → CAS ref (NodeResult.Files)
}

// runReduce executes a Map's reduce: clause AFTER fan-out, collapsing the N
// committed branch results into ONE output committed at nodePath (the map path).
// (Parallel-reduce is deferred — SP2 Task 8 — so nodePath is always a map path
// in SP2; runReduce is written path-agnostic so a future parallel wire-form can
// reuse it unchanged.) Returns the node's outcome. Two forms:
//
//   - quorum: count branches whose `over` field is true; ok iff ≥ k, committing
//     a synthetic {passed,votes,agree} typed output. A miss is retryable_failure
//     (mirrors min_success, engine/map.go) — NOT a new outcome class.
//   - run: stage every branch's named artifact + a canonical-JSON manifest into
//     the reducer's REQUIRED container (the SP1 CopyTo path), run the command,
//     commit its output_schema/output_files at nodePath.
//
// branches is the index-ordered list of (committed) branch results the caller
// collected (from LookupMapItems→LookupCompleted). The interpreter is the only
// state writer: runReduce Commits via the canonical engine.Commit and
// RecordCompleted, exactly like a step.
func runReduce(
	ctx context.Context,
	r *ir.Reduce,
	nodePath string,
	branches []reduceBranch,
	wf *ir.Workflow,
	rs *RunState,
	ld *LocalDispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
) (Outcome, error) {
	// Resume: a committed reduce replays.
	if _, ok := rs.LookupCompleted(nodePath); ok {
		return OutcomeOK, nil
	}
	switch {
	case r.IsQuorum():
		return runQuorumReduce(r, nodePath, branches, log, blobs, rs)
	case r.IsRun():
		return runCommandReduce(ctx, r, nodePath, branches, wf, rs, ld, log, blobs, clk, tap)
	default:
		return "", fmt.Errorf("engine.runReduce: reduce at %q has neither quorum nor run (validator AWF1035)", nodePath)
	}
}

// runQuorumReduce computes the quorum verdict purely in-engine and commits a
// synthetic {passed,votes,agree} typed output at nodePath. A not-met quorum is
// retryable_failure with no commit (mirrors min_success, engine/map.go).
func runQuorumReduce(r *ir.Reduce, nodePath string, branches []reduceBranch, log state.Log, blobs state.Blobs, rs *RunState) (Outcome, error) {
	agree := 0
	for _, b := range branches {
		if v, ok := b.Outputs[r.Over].(bool); ok && v {
			agree++
		}
	}
	need := quorumThreshold(r.Quorum, len(branches))
	passed := int64(agree) >= need
	out := map[string]any{"passed": passed, "votes": len(branches), "agree": agree}
	if !passed {
		// Mirror min_success: a not-met quorum is retryable_failure, no commit.
		return OutcomeRetryableFailure, fmt.Errorf("engine.runReduce: quorum %q: %d/%d branches agree, need %d",
			nodePath, agree, len(branches), need)
	}
	nr, err := Commit(log, blobs, nodePath, DispatchResult{Outcome: OutcomeOK, Outputs: out}, false)
	if err != nil {
		return "", fmt.Errorf("engine.runReduce: commit quorum at %q: %w", nodePath, err)
	}
	rs.RecordCompleted(nodePath, nr)
	return OutcomeOK, nil
}

// quorumThreshold reuses defaultMinSuccess's Ratio int/float interpretation so
// quorum and min_success are one parse. nil → all (defensive; validator
// requires quorum present).
func quorumThreshold(q *ir.Ratio, total int) int64 {
	tmp := &ir.Map{MinSuccess: q}
	return defaultMinSuccess(tmp, total)
}

// runCommandReduce stages the manifest + branch artifacts into the reducer's
// container and runs it as a synthesized CodeStep committing at nodePath.
func runCommandReduce(
	ctx context.Context, r *ir.Reduce, nodePath string, branches []reduceBranch,
	wf *ir.Workflow, rs *RunState, ld *LocalDispatcher, log state.Log, blobs state.Blobs, clk clock.Clock, tap io.Writer,
) (Outcome, error) {
	// 1. Canonical-JSON manifest of branch typed outputs (index-ordered).
	manifest := make([]map[string]any, 0, len(branches))
	for _, b := range branches {
		manifest = append(manifest, b.Outputs)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return failStep(log, nodePath, OutcomePermanentFailure, fmt.Errorf("marshal reduce manifest: %w", err))
	}
	canon, err := jcs.Transform(raw) // deterministic → resume-stable
	if err != nil {
		return failStep(log, nodePath, OutcomePermanentFailure, fmt.Errorf("canonicalize reduce manifest: %w", err))
	}
	inputs := []container.InputFile{{Path: reduceManifestPath, Content: canon}}

	// 2. Stage each branch's named artifacts (sorted dst for determinism).
	dsts := map[string][]byte{}
	for _, b := range branches {
		names := make([]string, 0, len(b.Files))
		for p := range b.Files {
			names = append(names, p)
		}
		sort.Strings(names)
		for _, p := range names {
			content, gerr := blobs.Get(b.Files[p])
			if gerr != nil {
				// Committed artifact unreadable — internal halt (SP1 errArtifactFetch precedent).
				return "", fmt.Errorf("engine.runReduce: %w: branch %d file %q: %v", errArtifactFetch, b.N, p, gerr)
			}
			dst := fmt.Sprintf("/work/.awf/branch-%d%s", b.N, ensureLeadingSlash(p))
			dsts[dst] = content
		}
	}
	sortedDsts := make([]string, 0, len(dsts))
	for d := range dsts {
		sortedDsts = append(sortedDsts, d)
	}
	sort.Strings(sortedDsts)
	for _, d := range sortedDsts {
		inputs = append(inputs, container.InputFile{Path: d, Content: dsts[d]})
	}

	// 3. Run as a synthesized CodeStep at nodePath. Reuse the dispatcher's
	//    runCode by building a NodeIntent with the reducer's Run/Container/
	//    OutputSchema/OutputFiles + the staged InputFiles. The dispatcher stages
	//    InputFiles via Backend.CopyTo (SP1) BEFORE exec — one delivery path.
	synth := &ir.CodeStep{Run: r.Run, Container: r.Container, OutputSchema: r.OutputSchema, OutputFiles: r.OutputFiles}
	resolved := ResolvedInputs{
		Command:      r.Run,
		Env:          map[string]string{},
		OutputFiles:  r.OutputFiles.Paths(),
		OutputSchema: r.OutputSchema,
		InputFiles:   inputs,
	}
	intent := NodeIntent{Path: nodePath, Node: synth, ResolvedInputs: resolved}

	appendNodeStarted(log, nodePath, "reduce")
	dr, chunks, runErr := RunWithRetry(ctx, ld, intent, retry.Default, clk, log)
	drainTap(chunks, "reduce", tap)
	if runErr != nil {
		if dr.Outcome == "" {
			return "", fmt.Errorf("engine.runReduce: dispatch at %q: %w", nodePath, runErr)
		}
		return failStep(log, nodePath, dr.Outcome, runErr)
	}
	nr, commitErr := Commit(log, blobs, nodePath, dr, false)
	if commitErr != nil {
		return "", fmt.Errorf("engine.runReduce: commit reduce at %q: %w", nodePath, commitErr)
	}
	rs.RecordCompleted(nodePath, nr)
	return OutcomeOK, nil
}

// ensureLeadingSlash normalizes a branch artifact's declared container path so
// the staged dst (/work/.awf/branch-<N><path>) is always well-formed.
func ensureLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p
	}
	return "/" + p
}
