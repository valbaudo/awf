package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gowebpki/jcs"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/state"
)

// reduceStagingDir is the in-container directory a run: reducer's manifest and
// per-branch artifacts are staged under (spec §3.2a). Single source of truth so
// the manifest path and the branch-artifact dst can never desync.
const reduceStagingDir = "/work/.awf"

// reduceManifestPath is where the canonical-JSON aggregate of branch typed
// outputs is staged in a run: reducer's container (spec §3.2a).
const reduceManifestPath = reduceStagingDir + "/aggregate.json"

// reduceBranch is one committed branch's contribution: its typed outputs + its
// named artifacts (declared container path → CAS ref), index-ordered by the
// caller (engine/map.go's collectReduceBranches, Task 11).
type reduceBranch struct {
	N       int
	Outputs map[string]any
	Files   map[string]string // declared container path → CAS ref (NodeResult.Files)
}

// runMapReduce runs a map's reduce: phase after fan-out (SP2 C2a): it collects
// the committed branch outputs+artifacts and collapses them via runReduce,
// committing the reduced output at the map's OWN path (the aggregate stays
// engine-internal; the reduced output replaces it). Extracted from runMap so the
// map handler ends with fan-out + tally and the fan-IN lives beside runReduce.
// cohort is the non-pruned fan-out count (the quorum denominator).
//
// Resume short-circuit FIRST: a committed reduce replays. Mirror runMap's
// per-item committed-skip — on a pure replay we must NOT boot (and tear down) the
// reducer container, which would turn a should-be-pure replay into real work that
// can FAIL the resume (e.g. an image no longer pullable), violating "committed
// steps are replayed, not recomputed; infra is rebuilt only for the uncommitted
// frontier." runReduce has the same guard, but it fires AFTER the Create below,
// so check it here before any infra.
func runMapReduce(
	ctx context.Context,
	n *ir.Map,
	mapPath string,
	cohort int,
	wf *ir.Workflow,
	runstate *RunState,
	ld *LocalDispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
) (Outcome, error) {
	if _, ok := runstate.LookupCompleted(mapPath); ok {
		return OutcomeOK, nil
	}
	branches := collectReduceBranches(runstate, n, mapPath)
	if n.Reduce.IsRun() {
		// A run: reducer is a code step → it needs its own container. Create it
		// here and Destroy after (mirrors dispatchItem's Create/defer Destroy).
		spec := ContainerSpecFor(wf, ld.ComposeFiles, n.Reduce.Container)
		rh, cerr := ld.Backend.Create(ctx, spec)
		if cerr != nil {
			return "", fmt.Errorf("engine.runMapReduce: create reduce container %q: %w", n.Reduce.Container, cerr)
		}
		defer func() { _ = ld.Backend.Destroy(context.Background(), rh) }()
		ld = ld.WithItemHandle(n.Reduce.Container, rh)
	}
	return runReduce(ctx, n.Reduce, mapPath, branches, cohort, wf, runstate, ld, log, blobs, clk, tap)
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
// collected (from LookupMapItems→LookupCompleted) — the SURVIVORS only; a branch
// that failed mechanically is absent. cohort is the NON-PRUNED fan-out count
// (len(over) minus the items the prune frontier discarded): the quorum threshold
// k is measured against the cohort, NOT the survivor count, so an explicit quorum
// cannot be silently satisfied by fewer agreeing branches than the author
// demanded when some branches crash. A mechanically-FAILED branch stays in the
// cohort (it is absent from `branches`, so it correctly counts as a non-agreeing
// vote); a deliberately-PRUNED item is removed from the cohort, exactly as it is
// removed from the min_success denominator (the two thresholds stay symmetric).
// The interpreter is the only state writer: runReduce Commits via the canonical
// engine.Commit and RecordCompleted, exactly like a step.
func runReduce(
	ctx context.Context,
	r *ir.Reduce,
	nodePath string,
	branches []reduceBranch,
	cohort int,
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
		return runQuorumReduce(r, nodePath, branches, cohort, log, blobs, rs)
	case r.IsRun():
		return runCommandReduce(ctx, r, nodePath, branches, wf, rs, ld, log, blobs, clk, tap)
	default:
		return "", fmt.Errorf("engine.runReduce: reduce at %q has neither quorum nor run (validator AWF1035)", nodePath)
	}
}

// runQuorumReduce computes the quorum verdict purely in-engine and commits a
// synthetic {passed,votes,agree} typed output at nodePath. A not-met quorum is
// retryable_failure with no commit (mirrors min_success, engine/map.go).
//
// The threshold k is measured against the NON-PRUNED fan-out COHORT, not the
// survivor count: a branch that failed mechanically is absent from branches and
// therefore correctly counts as a NON-agreeing vote. This is what makes
// "unanimous is quorum(N)" honest — a unanimous quorum over a cohort with one
// crashed branch FAILS (need=N over the cohort, agree<N), rather than passing on
// the survivors. (Counting agree over survivors but k over the cohort is the only
// combination that preserves the author's demand under crashes.) A deliberately-
// PRUNED item is NOT in the cohort the caller passes (engine/map.go subtracts the
// pruned count), so it neither inflates need nor counts as a missing vote —
// symmetric with min_success. votes reports the cohort size so the verdict reads
// against one denominator.
func runQuorumReduce(r *ir.Reduce, nodePath string, branches []reduceBranch, cohort int, log state.Log, blobs state.Blobs, rs *RunState) (Outcome, error) {
	agree := 0
	for _, b := range branches {
		if v, ok := b.Outputs[r.Over].(bool); ok && v {
			agree++
		}
	}
	need := quorumThreshold(r.Quorum, cohort)
	passed := int64(agree) >= need
	out := map[string]any{"passed": passed, "votes": cohort, "agree": agree}
	if !passed {
		// Mirror min_success: a not-met quorum is retryable_failure, no commit.
		return OutcomeRetryableFailure, fmt.Errorf("engine.runReduce: quorum %q: %d/%d branches agree, need %d",
			nodePath, agree, cohort, need)
	}
	nr, err := Commit(log, blobs, nodePath, DispatchResult{Outcome: OutcomeOK, Outputs: out}, false)
	if err != nil {
		return "", fmt.Errorf("engine.runReduce: commit quorum at %q: %w", nodePath, err)
	}
	rs.RecordCompleted(nodePath, nr)
	return OutcomeOK, nil
}

// quorumThreshold reuses ratioThreshold's Ratio int/float interpretation so
// quorum and min_success are one parse. cohort is the fan-out count the
// threshold is measured against (NOT the survivor count). nil → all (defensive;
// validator requires quorum present).
//
// NOTE: ratioThreshold caps an int threshold at its `total` argument
// (`if i > total { return total }`). That cap is correct ONLY when total is the
// cohort size — passing the survivor count would silently lower an explicit
// quorum k to the number of branches that happened to survive, letting a quorum
// pass with fewer agreeing branches than the author demanded.
func quorumThreshold(q *ir.Ratio, cohort int) int64 {
	return ratioThreshold(q, cohort)
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
			dst := fmt.Sprintf("%s/branch-%d%s", reduceStagingDir, b.N, ensureLeadingSlash(p))
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

// collectReduceBranches gathers the index-ordered, committed branch
// contributions for a Map's reduce: clause (Task 11). For every PASSED map item
// N (in MapItems[mapPath]), it reads the body step(s)' committed NodeResult at
// the per-item runtime path (ItemPath(mapPath,N) + "." + <bodyStepSuffix>) and
// merges their typed Outputs + named Files into one reduceBranch. This is the
// SAME committed data engine/scope.go's aggregateMapOutputs lifts (the spec's
// "from the existing aggregateMapOutputs" guarantee — deterministic,
// committed-items-only, index-ordered → resume-stable). Failed/uncommitted items
// are compacted out (they have no committed body output), exactly like the
// aggregate.
//
// The supported (single-producing-step) body shape yields one body NodeResult
// per item; if a body has multiple producing steps, their Outputs/Files are
// shallow-merged into the branch (distinct step ids → distinct keys, so no
// collision in practice).
func collectReduceBranches(rs *RunState, n *ir.Map, mapPath string) []reduceBranch {
	// Body step suffixes (the path tail after ".body."), in walk order.
	var suffixes []string
	ir.WalkNodes(n.Body, "body", func(node ir.Node, path string) {
		switch node.(type) {
		case *ir.CodeStep, *ir.AgentStep:
			// path is "body.<...>"; the per-item runtime path drops the leading
			// "body." (replaced by ".item-N"), so strip it to the suffix.
			suffixes = append(suffixes, strings.TrimPrefix(path, "body."))
		}
	})

	items := rs.LookupMapItems(mapPath) // shallow copy — safe to sort in place
	sort.Slice(items, func(i, j int) bool { return items[i].N < items[j].N })

	branches := make([]reduceBranch, 0, len(items))
	for _, mr := range items {
		if mr.Status != ItemPassed {
			continue // compact: only committed-success branches contribute
		}
		b := reduceBranch{N: mr.N, Outputs: map[string]any{}, Files: map[string]string{}}
		committed := false
		for _, suffix := range suffixes {
			nr, ok := rs.LookupCompleted(ItemStepPath(mapPath, mr.N, suffix))
			if !ok {
				continue
			}
			committed = true
			for k, v := range nr.Outputs {
				b.Outputs[k] = v
			}
			for k, v := range nr.Files {
				b.Files[k] = v
			}
		}
		if !committed {
			continue // no committed body output for this item — compact out
		}
		branches = append(branches, b)
	}
	return branches
}

// ensureLeadingSlash normalizes a branch artifact's declared container path so
// the staged dst (/work/.awf/branch-<N><path>) is always well-formed.
func ensureLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p
	}
	return "/" + p
}
