package engine

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/valbaudo/awf/template"
)

// reduceDefaultStagingDir is the fallback staging root for backends that
// predate StagingRoot (zero value). All current backends set it explicitly.
const reduceDefaultStagingDir = "/work/.awf"

// reduceStagingRoot returns the staging root for the given backend.
// On docker (StagingRoot: "/work/.awf") it is the in-container absolute path.
// On native (StagingRoot: ".awf") it is workdir-relative: native.CopyTo joins
// relative paths to the container workdir so no literal "/work/.awf" is created.
func reduceStagingRoot(b container.Backend) string {
	if root := b.Capabilities().StagingRoot; root != "" {
		return root
	}
	return reduceDefaultStagingDir
}

// reduceBranch is one committed branch's contribution: its typed outputs + its
// named artifacts (artifact name → CAS ref), index-ordered by the caller
// (engine/map.go's collectReduceBranches, Task 11).
type reduceBranch struct {
	N       int
	Outputs map[string]any
	Files   map[string]string // artifact name → CAS ref
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
	moduleID string,
	runstate *RunState,
	runEnv map[string]string,
	ld *LocalDispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	cc reduceCallContext,
) (Outcome, error) {
	if _, ok := runstate.LookupCompleted(mapPath); ok {
		return OutcomeOK, nil
	}
	branches, berr := collectReduceBranches(runstate, n, mapPath, wf)
	if berr != nil {
		if errors.Is(berr, errArtifactFetch) {
			return "", fmt.Errorf("engine.runMapReduce: collect reduce branches at %q: %w", mapPath, berr)
		}
		return failStep(log, mapPath, OutcomePermanentFailure, berr)
	}
	if n.Reduce.IsRun() {
		// A run reducer is a code step → it needs its container handle. The
		// reducer's container is a `containers:`-declared name, so it is normally
		// ALREADY provisioned and present in ld.Handles (brought up once at run
		// start, like every other declared container). REUSE that handle.
		//
		// Create-then-defer-Destroy here (the old behaviour, which "mirrored"
		// dispatchItem's per-ITEM ephemeral containers) is wrong for a SHARED
		// declared container: for a compose project it brings the project up a
		// second time AND tears the WHOLE project down when the reduce returns —
		// destroying a lab that LATER steps still use. That is the slice5
		// item4-reduce → item5 "Exec: unknown handle <lab>" failure; slice2 never
		// hit it because nothing ran after its reduce. Only a container the
		// dispatcher does NOT already hold (not pre-provisioned) is Created +
		// Destroyed here.
		bare, _ := SplitContainerRef(n.Reduce.Container)
		handleKey := ld.handleKey(bare)
		if _, have := ld.Handles[handleKey]; !have {
			spec := ContainerSpecFor(wf, ld.ComposeFiles, bare)
			spec.Name = handleKey
			rh, cerr := ld.Backend.Create(ctx, spec)
			if cerr != nil {
				return "", fmt.Errorf("engine.runMapReduce: create reduce container %q: %w", n.Reduce.Container, cerr)
			}
			defer func() { _ = ld.Backend.Destroy(context.Background(), rh) }()
			ld = ld.WithItemHandle(bare, rh)
		}
	}
	return runReduce(ctx, n.Reduce, mapPath, branches, cohort, wf, moduleID, runstate, runEnv, ld, log, blobs, clk, tap, cc)
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
	moduleID string,
	rs *RunState,
	runEnv map[string]string,
	ld *LocalDispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	cc reduceCallContext,
) (Outcome, error) {
	// Resume: a committed reduce replays.
	if _, ok := rs.LookupCompleted(nodePath); ok {
		return OutcomeOK, nil
	}
	switch {
	case r.IsQuorum():
		return runQuorumReduce(r, nodePath, branches, cohort, log, blobs, rs)
	case r.IsRun():
		return runCommandReduce(ctx, r, nodePath, branches, wf, moduleID, rs, runEnv, ld, log, blobs, clk, tap, cc)
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
		if v, ok := b.Outputs[r.Field].(bool); ok && v {
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
	wf *ir.Workflow, moduleID string, rs *RunState, runEnv map[string]string, ld *LocalDispatcher, log state.Log, blobs state.Blobs, clk clock.Clock, tap io.Writer,
	cc reduceCallContext,
) (Outcome, error) {
	// Derive the per-backend staging root (docker: "/work/.awf", native: ".awf").
	stagingRoot := reduceStagingRoot(ld.Backend)
	manifestPath := stagingRoot + "/aggregate.json"

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
	inputs := []container.InputFile{{Path: manifestPath, Content: canon}}

	// 2. Stage each branch's named artifacts (sorted dst for determinism).
	dsts := map[string][]byte{}
	for _, b := range branches {
		names := make([]string, 0, len(b.Files))
		for name := range b.Files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			content, gerr := blobs.Get(b.Files[name])
			if gerr != nil {
				// Committed artifact unreadable — internal halt (SP1 errArtifactFetch precedent).
				return "", fmt.Errorf("engine.runReduce: %w: branch %d file %q: %v", errArtifactFetch, b.N, name, gerr)
			}
			dst := fmt.Sprintf("%s/branch-%d/%s", stagingRoot, b.N, name)
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
	// Template reducer run/output_files against the reducer scope. It behaves like
	// the map-path scope for ordinary refs, and renders body-step aggregate refs as
	// canonical JSON for the historical reducer-template contract.
	reduceScope := newReduceTemplateScopeForExec(rs, wf, nodePath, cc)
	cmd, terr := template.Substitute(r.Run, reduceScope)
	if terr != nil {
		return failStep(log, nodePath, OutcomePermanentFailure, fmt.Errorf("engine.runReduce: template reduce run %q: %w", r.Run, terr))
	}
	outputFiles, outputFileContracts, oerr := resolveOutputFiles(r.OutputFiles, reduceScope, moduleID, rs.Assets, blobs)
	if oerr != nil {
		if errors.Is(oerr, errArtifactFetch) {
			return "", fmt.Errorf("engine.runReduce: resolve output_files contracts at %q: %w", nodePath, oerr)
		}
		return failStep(log, nodePath, OutcomePermanentFailure, fmt.Errorf("engine.runReduce: substitute output_files at %q: %w", nodePath, oerr))
	}
	synth := &ir.CodeStep{Run: cmd, Container: r.Container, OutputSchema: r.OutputSchema, OutputFiles: r.OutputFiles}
	// I1: forward the resolved workflow env: allowlist (F15), like a graph run:
	// step — copy FIRST, then set the engine key on top so AWF_STAGING_ROOT
	// always wins a name collision with an author-declared env: name.
	env := copyRunEnv(runEnv)
	env["AWF_STAGING_ROOT"] = stagingRoot
	resolved := ResolvedInputs{
		Command:             cmd,
		Env:                 env,
		OutputFiles:         outputFiles,
		OutputFileContracts: outputFileContracts,
		OutputSchema:        r.OutputSchema,
		InputFiles:          inputs,
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
// engine/scope.go's aggregateMapOutputs intentionally does NOT need the same
// gate-nested-producer handling: ir.SingleMapBodyShape rejects any path
// containing a "gate[" or "loop[" segment, so a gate-nested producer is
// structurally unreachable from the typed-aggregate path.
//
// The supported (single-producing-step) body shape yields one body NodeResult
// per item; if a body has multiple producing steps, their Outputs/Files are
// shallow-merged into the branch. Files are keyed by declared output_files name,
// matching the reducer staging contract ($AWF_STAGING_ROOT/branch-N/<name>).
func collectReduceBranches(rs *RunState, n *ir.Map, mapPath string, wf *ir.Workflow) ([]reduceBranch, error) {
	// Body step suffixes (the path tail after ".body."), in walk order.
	var producers []reduceBodyProducer
	ir.WalkNodes(n.Body, "body", func(node ir.Node, path string) {
		switch s := node.(type) {
		case *ir.CodeStep:
			// path is "body.<...>"; the per-item runtime path drops the leading
			// "body." (replaced by ".item-N"), so strip it to the suffix.
			producers = append(producers, reduceBodyProducer{
				suffix:      strings.TrimPrefix(path, "body."),
				outputFiles: s.OutputFiles,
			})
		case *ir.AgentStep:
			producers = append(producers, reduceBodyProducer{
				suffix:      strings.TrimPrefix(path, "body."),
				outputFiles: s.OutputFiles,
			})
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
		for _, producer := range producers {
			itemStepPath, gateForwarded, resolved := itemBodyStepPath(rs, mapPath, mr.N, producer.suffix)
			if !resolved {
				continue // gate-nested producer with no passed attempt for this item
			}
			nr, ok := rs.LookupCompleted(itemStepPath)
			if !ok {
				continue
			}
			committed = true
			if !gateForwarded {
				// Gate SCALARS stay gate-scoped (man:744-747); only files forward.
				for k, v := range nr.Outputs {
					b.Outputs[k] = v
				}
			}
			stepScope := NewScope(rs, wf, itemStepPath)
			for _, of := range producer.outputFiles {
				if of.Name == "" {
					continue
				}
				containerPath, err := template.Substitute(of.Path, stepScope)
				if err != nil {
					return nil, fmt.Errorf("engine.collectReduceBranches: branch %d output_files.%s path %q: %w", mr.N, of.Name, of.Path, err)
				}
				ref, ok := nr.Files[containerPath]
				if !ok {
					return nil, fmt.Errorf("%w: branch %d output_files.%s not committed at %q", errArtifactFetch, mr.N, of.Name, containerPath)
				}
				b.Files[of.Name] = ref
			}
		}
		if !committed {
			continue // no committed body output for this item — compact out
		}
		branches = append(branches, b)
	}
	return branches, nil
}

type reduceBodyProducer struct {
	suffix      string
	outputFiles ir.OutputFiles
}
