package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

// ErrNodeNotImplemented is the sentinel the interpreter returns for any node
// kind not yet implemented in the current runtime. Phase 5 slice 5.2 closes
// the last remaining case (AgentStep / uses:) — all graph node kinds are now
// implemented. The sentinel is retained as a public export because the CLI
// layer (cli/execute.go) maps it to an exit code, and removing it would be a
// breaking API change.
//
// Distinct from engine.ErrUnsupportedKind (the dispatcher's per-step
// sentinel) — the two answer different questions.
//
// Wrap with kind + path for diagnostic clarity:
//
//	fmt.Errorf("%w: %s at path %q", ErrNodeNotImplemented, kindName, path)
var ErrNodeNotImplemented = errors.New("engine: node kind not implemented")

// RunOptions carries optional runtime hooks for Run.
type RunOptions struct {
	// Tap, if non-nil, receives the live-tap output: one "[step.id] <chunk>" per
	// IOChunk produced by each step. nil disables the tap entirely.
	Tap io.Writer

	// Broker is the signal broker for pause/cancel IPC. nil is valid — signal
	// steps fail with a clear error when reached with a nil broker.
	Broker *signal.Broker

	// Assets is the recorded run-start asset manifest. Task 5 consumes it when
	// resolving asset.<id> input_files; nil preserves pre-assets behavior.
	Assets map[string]RunStartedAsset
}

// Run is the top-level interpreter entry point. Walks def.Workflow.Graph
// recursively; for each node, computes its runtime path; consults runstate to
// skip committed nodes (the resume invariant — slice 2.6 exercises the
// non-empty-Completed case); otherwise dispatches per-kind. Halts on the first
// error or non-ok outcome that escapes any try/catch enclosing it (slice 3.1
// added try/catch; future slices add gate/parallel/map — each defines its own
// absorption rule per Phase 3 spec §5).
//
// Returns:
//   - (OutcomeOK, nil)                              — every node committed ok,
//     OR a Skip unwound to the
//     workflow root (spec §5.6)
//   - (OutcomeRetryableFailure | OutcomePermanentFailure, <step's err>)
//     — a step terminated; the
//     run.finished event records
//     the same outcome
//   - ("", <internal-error>)                        — interpreter / CLI bug
//     (unknown container,
//     ErrNodeNotImplemented,
//     log/blobs failure). The CLI
//     distinguishes this from
//     step failures via the empty
//     Outcome (slice 2.5 DQ4).
//
// dispatcher's Handles map MUST be populated for every container the workflow
// declares; engine.Run is NOT responsible for Backend.Create / Destroy (the
// CLI owns lifecycle, slice 2.5 DQ3). An unknown-container reference inside
// a step is an internal error.
func Run(
	ctx context.Context,
	def *ir.LoadedDefinition,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	opts RunOptions,
) (Outcome, error) {
	if def == nil || def.Workflow == nil {
		return "", fmt.Errorf("engine.Run: nil workflow")
	}
	if runstate == nil {
		return "", fmt.Errorf("engine.Run: nil runstate")
	}
	if opts.Assets != nil {
		runstate.Assets = opts.Assets
	}

	// Wrap ctx so the background poller can cancel it on pause/cancel
	// detection. The deferred cancel() ensures the poller exits when
	// engine.Run returns normally (no pause/cancel detected).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Background polling goroutine (slice 3.5). Skip if broker is nil
	// (tests / no-signal workflows).
	pollDone := make(chan struct{})
	if opts.Broker != nil {
		go pollControls(runCtx, opts.Broker, runstate, cancel, pollDone)
	} else {
		close(pollDone)
	}

	oc, err := interpNodes(runCtx, def.Workflow.Graph, "", def.Workflow, runstate, dispatcher, log, blobs, clk, opts.Tap, opts.Broker)

	// SkipUnwind reaching Run = workflow-root unwind target (no enclosing
	// loop/try/parallel/gate/map caught it). Per Phase 3 spec §5.6: "Cleanly
	// terminates the nearest enclosing scope ... or (if none) the run, as ok."
	// The skip might coincide with a pending pause/cancel; the pause/cancel
	// checks below take precedence (terminal beats observational).
	var su *SkipUnwind
	if errors.As(err, &su) {
		// Append node.skipped{path: "", reason} for trace (Phase 6 obs).
		if appendErr := appendNodeSkipped(log, "", su.Reason); appendErr != nil {
			return "", appendErr
		}
		oc, err = OutcomeOK, nil
	}

	// Ensure the poller has exited before we read the flags. cancel() is
	// idempotent — calling it again here covers the case where interpNodes
	// returned via something OTHER than the poller cancelling (normal
	// completion or step failure).
	cancel()
	<-pollDone

	// Post-walk event emission via the appendTerminalControlEvents helper
	// (H1 fix — extracted from engine.Run for unit-testability). Cancel
	// takes precedence over pause; both take precedence over the natural
	// (oc, err) return.
	if termErr := appendTerminalControlEvents(log, runstate); termErr != nil {
		return "", termErr
	}
	return oc, err
}

// interpNodes recursively walks a NodeList in order. Each node's path is
// computed via ir.PathFor — the single source of truth for journal keys
// (CLAUDE.md "node addressing is one pure function" invariant; runtime
// suffixes — iter-N for Loop body — are added inside the loop handler, not
// here).
func interpNodes(
	ctx context.Context,
	nodes ir.NodeList,
	parent string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	broker *signal.Broker,
) (Outcome, error) {
	for i, n := range nodes {
		oc, err := interpNode(ctx, n, i, parent, wf, runstate, dispatcher, log, blobs, clk, tap, broker)
		if oc != OutcomeOK || err != nil {
			return oc, err
		}
	}
	return OutcomeOK, nil
}

// interpNode handles one node. Kind-dispatch lives here so each recursive call
// site shares the same per-kind switch.
func interpNode(
	ctx context.Context,
	n ir.Node,
	idx int,
	parent string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	broker *signal.Broker,
) (Outcome, error) {
	switch v := n.(type) {
	case *ir.CodeStep:
		return runCodeStep(ctx, v, ir.PathFor(parent, "", v.ID, idx), wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case *ir.If:
		return runIf(ctx, v, ir.PathFor(parent, "if", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case *ir.Loop:
		return runLoop(ctx, v, ir.PathFor(parent, "loop", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case *ir.AgentStep:
		return runAgentStep(ctx, v, ir.PathFor(parent, "", v.ID, idx), wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case *ir.SignalStep:
		return runSignalStep(ctx, v, ir.PathFor(parent, "", v.ID, idx), wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case *ir.Try:
		return runTry(ctx, v, ir.PathFor(parent, "try", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case *ir.Parallel:
		return runParallel(ctx, v, ir.PathFor(parent, "parallel", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case *ir.Gate:
		return runGate(ctx, v, ir.PathFor(parent, "gate", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case *ir.Map:
		return runMap(ctx, v, ir.PathFor(parent, "map", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case *ir.Compose:
		return runCompose(ctx, v, ir.PathFor(parent, "compose", "", idx), wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case *ir.Skip:
		return runSkip(v)
	default:
		return "", fmt.Errorf("engine: unknown node type %T at parent %q index %d (validator should have caught)", n, parent, idx)
	}
}

// errArtifactFetch marks a Blobs.Get failure during input_files staging. Unlike
// a ref-resolution error (author bug → permanent_failure), a fetch failure of a
// committed, content-addressed artifact is corruption/IO territory — the caller
// halts as an internal error (matching engine/fold.go + signal_step), so resume
// re-runs the uncommitted step and re-fetches. (In-run staging retry is a
// future enhancement; out of scope for SP1's local single-host CAS.) Caveat:
// this surfaces with the same exit class as an interpreter bug (the "" outcome),
// consistent with fold.go — a missing/corrupt committed blob is a data/infra
// fault, not a step outcome; we do not invent a new outcome class for it.
var errArtifactFetch = errors.New("engine: input_files artifact fetch failed")

// resolveInputFiles maps a step's input_files (container-path → artifact/asset ref)
// to staged bytes. Destination container paths are template-substituted against
// the consumer's scope before staging, matching output_files path templating.
// Builds the name→path index once per call (one graph walk via
// ir.OutputFilesByStepID), avoiding a re-walk per ref within the call.
// Ref errors (parse/undeclared/not-committed) return a plain error (caller →
// permanent_failure); a Blobs.Get failure is wrapped with errArtifactFetch
// (caller → internal halt). Sorted by dst for determinism.
func resolveInputFiles(in map[string]string, scope *Scope, wf *ir.Workflow, blobs state.Blobs, assets map[string]RunStartedAsset) ([]container.InputFile, error) {
	if len(in) == 0 {
		return nil, nil
	}
	idx := ir.OutputFilesByStepID(wf)
	dsts := make([]string, 0, len(in))
	for d := range in {
		dsts = append(dsts, d)
	}
	sort.Strings(dsts)
	expanded := make([]resolvedInputFile, 0, len(in))
	for _, dst := range dsts {
		resolvedDst, err := template.Substitute(dst, scope)
		if err != nil {
			return nil, fmt.Errorf("input_files[%s]: substitute destination: %w", dst, err)
		}
		if !path.IsAbs(resolvedDst) || resolvedDst != path.Clean(resolvedDst) {
			return nil, fmt.Errorf("input_files[%s]: substituted destination %q must be an absolute, clean path (no '..' segment)", dst, resolvedDst)
		}
		rawRef := in[dst]
		if id, ok := template.ParseAssetRef(rawRef); ok {
			files, err := resolveAssetInputFiles(resolvedDst, rawRef, id, assets, blobs)
			if err != nil {
				return nil, fmt.Errorf("input_files[%s]: %w", dst, err)
			}
			expanded = append(expanded, files...)
			continue
		}
		id, name, ok := template.ParseArtifactRef(rawRef)
		if !ok {
			return nil, fmt.Errorf("input_files[%s]=%q: expected step.<id>.files.<name> or asset.<id>", dst, rawRef)
		}
		declaredPath, ok := idx[id].PathForName(name)
		if !ok {
			return nil, fmt.Errorf("input_files[%s]: step %q has no named output_files artifact %q", dst, id, name)
		}
		cas, err := scope.ResolveDeclaredArtifactPath(id, declaredPath)
		if err != nil {
			return nil, fmt.Errorf("input_files[%s]: %w", dst, err)
		}
		b, err := blobs.Get(cas)
		if err != nil {
			return nil, fmt.Errorf("input_files[%s]: %w (%v)", dst, errArtifactFetch, err)
		}
		expanded = append(expanded, resolvedInputFile{
			file:   container.InputFile{Path: resolvedDst, Content: b},
			source: rawRef,
		})
	}
	if err := rejectInputFilePathCollisions(expanded); err != nil {
		return nil, err
	}
	out := make([]container.InputFile, 0, len(expanded))
	for _, e := range expanded {
		out = append(out, e.file)
	}
	return out, nil
}

type resolvedInputFile struct {
	file   container.InputFile
	source string
}

func resolveAssetInputFiles(dst, rawRef, id string, assets map[string]RunStartedAsset, blobs state.Blobs) ([]resolvedInputFile, error) {
	asset, ok := assets[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s: asset %q was not recorded in run.started", errArtifactFetch, rawRef, id)
	}
	if !asset.IsDir {
		if len(asset.Files) != 1 || asset.Files[0].Path != "." {
			return nil, fmt.Errorf("%w: %s: file asset %q has invalid run-start manifest", errArtifactFetch, rawRef, id)
		}
		b, err := readRunStartedAssetFile(blobs, asset.Files[0])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rawRef, err)
		}
		return []resolvedInputFile{{
			file:   container.InputFile{Path: dst, Content: b},
			source: rawRef,
		}}, nil
	}
	files := append([]RunStartedAssetFile(nil), asset.Files...)
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: %s: directory asset %q has empty run-start manifest", errArtifactFetch, rawRef, id)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	out := make([]resolvedInputFile, 0, len(files))
	for _, f := range files {
		if !validAssetManifestPath(f.Path) {
			return nil, fmt.Errorf("%w: %s: directory asset %q has unsafe manifest path %q", errArtifactFetch, rawRef, id, f.Path)
		}
		b, err := readRunStartedAssetFile(blobs, f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rawRef, err)
		}
		out = append(out, resolvedInputFile{
			file:   container.InputFile{Path: path.Join(dst, f.Path), Content: b},
			source: rawRef,
		})
	}
	return out, nil
}

func validAssetManifestPath(p string) bool {
	return p != "" && p != "." && !path.IsAbs(p) && p == path.Clean(p) && p != ".." && !strings.HasPrefix(p, "../")
}

func readRunStartedAssetFile(blobs state.Blobs, file RunStartedAssetFile) ([]byte, error) {
	b, err := blobs.Get(file.Ref)
	if err != nil {
		return nil, fmt.Errorf("%w: asset file %q ref %q: %v", errArtifactFetch, file.Path, file.Ref, err)
	}
	if int64(len(b)) != file.Size {
		return nil, fmt.Errorf("%w: asset file %q ref %q size %d != recorded %d", errArtifactFetch, file.Path, file.Ref, len(b), file.Size)
	}
	sum := sha256.Sum256(b)
	got := hex.EncodeToString(sum[:])
	if got != file.SHA256 {
		return nil, fmt.Errorf("%w: asset file %q ref %q sha256 %s != recorded %s", errArtifactFetch, file.Path, file.Ref, got, file.SHA256)
	}
	return b, nil
}

func rejectInputFilePathCollisions(files []resolvedInputFile) error {
	sort.Slice(files, func(i, j int) bool {
		if files[i].file.Path == files[j].file.Path {
			return files[i].source < files[j].source
		}
		return files[i].file.Path < files[j].file.Path
	})
	for i := 1; i < len(files); i++ {
		prev := files[i-1]
		cur := files[i]
		if prev.file.Path == cur.file.Path {
			return fmt.Errorf("input_files expanded paths collide: %s from %s and %s from %s", prev.file.Path, prev.source, cur.file.Path, cur.source)
		}
		if isInputFileAncestor(prev.file.Path, cur.file.Path) {
			return fmt.Errorf("input_files expanded paths collide: %s from %s is an ancestor of %s from %s", prev.file.Path, prev.source, cur.file.Path, cur.source)
		}
	}
	return nil
}

func isInputFileAncestor(parent, child string) bool {
	if parent == "/" {
		return child != "/"
	}
	return strings.HasPrefix(child, parent+"/")
}

// resolveOutputFiles template-substitutes each output_files path against the
// step's scope, returning capture paths plus any contract metadata keyed by the
// substituted capture path. output_files paths are templated exactly like run:
// and idempotency_key, so a path such as /work/records/{{ input.cve_id }}.json
// captures — and commits, PATH-keyed in commit.go — under the substituted name.
func resolveOutputFiles(ofs ir.OutputFiles, scope template.Scope, assets map[string]RunStartedAsset, blobs state.Blobs) ([]string, map[string]OutputFileContract, error) {
	if len(ofs) == 0 {
		return nil, nil, nil
	}
	paths := make([]string, 0, len(ofs))
	contracts := map[string]OutputFileContract{}
	for _, of := range ofs {
		p, err := template.Substitute(of.Path, scope)
		if err != nil {
			return nil, nil, fmt.Errorf("output_files path %q: %w", of.Path, err)
		}
		paths = append(paths, p)
		contract, hasContract, err := resolveOutputFileContract(of, assets, blobs)
		if err != nil {
			return nil, nil, err
		}
		if hasContract {
			contracts[p] = contract
		}
	}
	if len(contracts) == 0 {
		contracts = nil
	}
	return paths, contracts, nil
}

func resolveOutputFileContract(of ir.OutputFile, assets map[string]RunStartedAsset, blobs state.Blobs) (OutputFileContract, bool, error) {
	if of.Format == "" && of.Schema == nil && of.SchemaRef == "" {
		return OutputFileContract{}, false, nil
	}
	if of.Path == "" {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: contract object requires path", of.Name)
	}
	if of.Format != "" && of.Format != "json" && of.Format != "jsonl" {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: format must be json or jsonl", of.Name)
	}
	if (of.Schema != nil || of.SchemaRef != "") && of.Format == "" {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema requires format json or jsonl", of.Name)
	}
	if of.Schema != nil && of.SchemaRef != "" {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema and schema_ref are mutually exclusive", of.Name)
	}
	contract := OutputFileContract{Format: of.Format, Schema: of.Schema}
	if of.Schema != nil {
		if _, err := compileJSONSchema(of.Schema); err != nil {
			return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema: %w", of.Name, err)
		}
	}
	if of.SchemaRef == "" {
		return contract, true, nil
	}
	id, ok := template.ParseAssetRef(of.SchemaRef)
	if !ok {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema_ref must be asset.<id>", of.Name)
	}
	asset, ok := assets[id]
	if !ok {
		return OutputFileContract{}, false, fmt.Errorf("%w: output_files.%s: schema_ref asset %q was not recorded in run.started", errArtifactFetch, of.Name, id)
	}
	if asset.IsDir || len(asset.Files) != 1 || asset.Files[0].Path != "." {
		return OutputFileContract{}, false, fmt.Errorf("%w: output_files.%s: schema_ref asset %q has invalid run-start manifest", errArtifactFetch, of.Name, id)
	}
	raw, err := readRunStartedAssetFile(blobs, asset.Files[0])
	if err != nil {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: %w", of.Name, err)
	}
	var schema ir.JSONSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema_ref asset %q is not JSON: %w", of.Name, id, err)
	}
	if _, err := compileJSONSchema(&schema); err != nil {
		return OutputFileContract{}, false, fmt.Errorf("output_files.%s: schema_ref asset %q: %w", of.Name, id, err)
	}
	contract.Schema = &schema
	return contract, true, nil
}

// runCodeStep is the CodeStep handler — composes substitution, retry, dispatch,
// classify, commit, AND failure-event emission. Each of those primitives is
// slice 2.4's; this function is the connecting tissue.
//
// Flow:
//
//  1. Resume-check — if path ∈ runstate.Completed, skip silently.
//  2. Build the template Scope rooted at this step's path; substitute cs.Run
//     and cs.IdempotencyKey. Template errors → node.failed{permanent_failure}
//     (slice 2.5 DQ7).
//  3. Merge retry policy: default + cs.Retry. Unknown backoff → internal error
//     halt (slice 2.4 DQ8).
//  4. Build NodeIntent + ResolvedInputs.
//  5. RunWithRetry — handles attempts + backoff + retry.attempt events.
//  6. On OK: drain the live-tap channel, then Commit. Update Completed[path].
//  7. On non-ok: drain the live-tap channel, then append node.failed{outcome,
//     error}. Return (outcome, err) — propagate up.
//  8. On internal error from RunWithRetry (unknown container, etc.): no
//     node.failed event (the step never got to fail mechanically — this is
//     the CLI's bug, not the step's). Return ("", err).
func runCodeStep(
	ctx context.Context,
	cs *ir.CodeStep,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	_ *signal.Broker, // carried for signature uniformity; runCodeStep does not recurse
) (Outcome, error) {
	if _, done := runstate.LookupCompleted(path); done {
		return OutcomeOK, nil
	}

	scope := NewScope(runstate, wf, path)
	command, err := template.Substitute(cs.Run, scope)
	if err != nil {
		return failStep(log, path, OutcomePermanentFailure, err)
	}
	idemKey := ""
	if cs.IdempotencyKey != nil {
		idemKey, err = template.Substitute(string(*cs.IdempotencyKey), scope)
		if err != nil {
			return failStep(log, path, OutcomePermanentFailure, err)
		}
	}

	policy, err := retry.Merge(retry.Default, cs.Retry)
	if err != nil {
		return "", fmt.Errorf("engine.Run: build retry policy at path %q: %w", path, err)
	}

	inputFiles, err := resolveInputFiles(cs.InputFiles, scope, wf, blobs, runstate.Assets)
	if err != nil {
		if errors.Is(err, errArtifactFetch) {
			// Committed artifact unreadable — internal error (content-address
			// invariant says it must exist); resume re-runs + re-fetches.
			return "", fmt.Errorf("engine.Run: stage input_files at %q: %w", path, err)
		}
		return failStep(log, path, OutcomePermanentFailure, err)
	}

	outputFiles, outputFileContracts, err := resolveOutputFiles(cs.OutputFiles, scope, runstate.Assets, blobs)
	if err != nil {
		if errors.Is(err, errArtifactFetch) {
			return "", fmt.Errorf("engine.Run: resolve output_files contracts at %q: %w", path, err)
		}
		return failStep(log, path, OutcomePermanentFailure, fmt.Errorf("engine.Run: substitute output_files at %q: %w", path, err))
	}
	snapBare, _ := SplitContainerRef(cs.Container)
	resolved := ResolvedInputs{
		Command:               command,
		Env:                   map[string]string{},
		OutputFiles:           outputFiles,
		OutputFileContracts:   outputFileContracts,
		OutputSchema:          cs.OutputSchema,
		NonRetryableExitCodes: policy.NonRetryableExitCodes,
		Snapshot:              wf.Containers[snapBare].Snapshot,
		InputFiles:            inputFiles,
	}
	if cs.Timeout != nil {
		resolved.Timeout = time.Duration(*cs.Timeout)
	}

	intent := NodeIntent{
		Path:           path,
		Node:           cs,
		ResolvedInputs: resolved,
		IdempotencyKey: idemKey,
	}

	appendNodeStarted(log, path, "code")

	dr, chunks, runErr := RunWithRetry(ctx, dispatcher, intent, policy, clk, log)
	// Drain the live-tap channel (single consumer — fine for Phase 2's
	// pre-closed fake channels; Phase 4's Docker streaming will require
	// this be moved to a goroutine).
	drainTap(chunks, cs.ID, tap)

	if runErr != nil {
		// dr.Outcome == "" means RunWithRetry never successfully dispatched (the
		// dispatcher itself returned a non-nil error on the first attempt — unknown
		// container, ErrUnsupportedKind, transport-classifier bug). No step outcome
		// was produced; treat as an internal error, no node.failed event.
		// Note: RunWithRetry can also return non-nil errors with a non-empty Outcome
		// (ctx cancellation mid-backoff, retry.attempt log failure). Those fall
		// through to failStep below — the last attempt's outcome is the right
		// classification, and node.failed records the underlying cause.
		if dr.Outcome == "" {
			return "", fmt.Errorf("engine.Run: dispatch at path %q: %w", path, runErr)
		}
		// Step outcome class: runErr is the underlying cause.
		return failStep(log, path, dr.Outcome, runErr)
	}

	if dr.Outcome != OutcomeOK {
		// Defensive — RunWithRetry should always return non-nil err on non-ok.
		return failStep(log, path, dr.Outcome, errors.New("step did not commit (no underlying error reported)"))
	}

	nr, err := Commit(log, blobs, path, dr, false) // code steps never participate in conversations
	if err != nil {
		return "", fmt.Errorf("engine.Run: commit at path %q: %w", path, err)
	}
	runstate.RecordCompleted(path, nr)
	return OutcomeOK, nil
}

// runIf is the If handler. Spec §5.1 + design spec §F: evaluate cond, append
// branch.taken, recurse into the chosen branch. Resume-safe: if
// runstate.LookupBranch(path) returns recorded=true, take the recorded
// branch without re-evaluating (the §8 "committed steps are replayed, not
// recomputed" invariant applies to the branch decision — re-evaluating
// could choose the OTHER branch if cond depends on a step output that's
// now in Completed).
//
// The child branch path is `path + "." + which` — a literal join (path is
// already "if[N]" via ir.PathFor at the interpNode dispatch). ir.ChildPath
// isn't used here because we already have the if's path computed; ChildPath
// is the parent→child computation for static analysis.
func runIf(
	ctx context.Context,
	n *ir.If,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	broker *signal.Broker,
) (Outcome, error) {
	which, recorded := runstate.LookupBranch(path)
	if !recorded {
		scope := NewScope(runstate, wf, path)
		// Template-error class (parse: AWF4005; eval: AWF4001/4002/4003/4004) —
		// DQ7: permanent_failure for the if NODE. The error is the author's bug.
		cond, err := template.EvalBoolString(string(n.Cond), scope)
		if err != nil {
			return failStep(log, path, OutcomePermanentFailure, err)
		}
		if cond {
			which = "then"
		} else {
			which = "else"
		}
		data, err := json.Marshal(BranchTakenData{Which: which})
		if err != nil {
			return "", fmt.Errorf("engine.Run: marshal branch.taken at path %q: %w", path, err)
		}
		if err := log.Append(state.Event{
			Type: EventBranchTaken,
			Path: path,
			Data: data,
		}); err != nil {
			return "", fmt.Errorf("engine.Run: append branch.taken at path %q: %w", path, err)
		}
		// branch.taken is observational — no Log.Sync (rides the next fsync per
		// spec §8 cost lever). A branch.taken lost to a torn tail means resume
		// re-evaluates the cond, which is correct first-run-equivalent behavior.
		runstate.RecordBranch(path, which)
	}

	switch which {
	case "then":
		return interpNodes(ctx, n.Then, path+".then", wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	case "else":
		// nil/empty Else is a no-op per spec §5.1.
		if len(n.Else) == 0 {
			return OutcomeOK, nil
		}
		return interpNodes(ctx, n.Else, path+".else", wf, runstate, dispatcher, log, blobs, clk, tap, broker)
	default:
		// Validator should reject; this is corruption / bug defense.
		return "", fmt.Errorf("engine.Run: unknown branch %q at path %q", which, path)
	}
}

// runLoop is the Loop handler. Spec §5.2 + design spec §F + slice 2.5 DQ8:
// do-while iteration. Body runs; loop.iter{n} appends ONLY after body succeeds
// (DQ8 invariant — body failure means iter never completed, no loop.iter
// event). until is tested AFTER each iter (so it can reference what the body
// just produced); max_iters bounds the loop. Resume-safe: K starts at
// runstate.LookupLoopIters(path) + 1.
//
// The validator (ir/validate_structural.go) enforces "at least one of until /
// max_iters" per spec §5.2. The runtime defends against validator regression
// by erroring loud if both are nil (slice 2.5 R7) — silent exit on iter 1
// would mask the validator bug.
func runLoop(
	ctx context.Context,
	n *ir.Loop,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	broker *signal.Broker,
) (Outcome, error) {
	// Defense-in-depth FIRST: avoid entering the loop with a definition that
	// validation should have rejected.
	if n.Until == nil && n.MaxIters == nil {
		return "", fmt.Errorf("engine: loop at path %q has neither until nor max_iters — validator regression (AWF §5.2 requires one); please report", path)
	}

	startK := runstate.LookupLoopIters(path) + 1
	for k := startK; ; k++ {
		bodyParent := IterPath(path+".body", k)
		// 1. Walk the body for iter K.
		oc, err := interpNodes(ctx, n.Body, bodyParent, wf, runstate, dispatcher, log, blobs, clk, tap, broker)
		// SkipUnwind from body = iteration end target. Append node.skipped for
		// trace, then continue to the loop.iter append below — the iter
		// completed (via skip) and loop.iter records that for resume.
		var su *SkipUnwind
		skipped := errors.As(err, &su)
		if skipped {
			if appendErr := appendNodeSkipped(log, bodyParent, su.Reason); appendErr != nil {
				return "", appendErr
			}
			// Clear oc/err to fall through to the loop.iter + until/max_iters
			// path — the iter is "complete" from the loop's perspective.
			oc, err = OutcomeOK, nil
		}
		if oc != OutcomeOK || err != nil {
			// Body failed — DQ8: do NOT emit loop.iter for this iter.
			return oc, err
		}
		// 2. Body completed. Append loop.iter{n: k}.
		data, mErr := json.Marshal(LoopIterData{N: k})
		if mErr != nil {
			return "", fmt.Errorf("engine.Run: marshal loop.iter at path %q iter %d: %w", path, k, mErr)
		}
		if err := log.Append(state.Event{
			Type: EventLoopIter,
			Path: path,
			Data: data,
		}); err != nil {
			return "", fmt.Errorf("engine.Run: append loop.iter at path %q iter %d: %w", path, k, err)
		}
		// loop.iter is observational — no Log.Sync (rides next fsync).
		runstate.RecordLoopIter(path, k)

		// 3. Evaluate until (if set). True → exit. Scope rooted at bodyParent so
		//    step.<id>.<field> refs resolve to THIS iter's outputs (spec §5.2
		//    "most recent iteration"). Template errors → permanent_failure (DQ7).
		if n.Until != nil {
			scope := NewScope(runstate, wf, bodyParent)
			done, evalErr := template.EvalBoolString(string(*n.Until), scope)
			if evalErr != nil {
				return failStep(log, path, OutcomePermanentFailure, evalErr)
			}
			if done {
				return OutcomeOK, nil
			}
		}

		// 4. Check max_iters. K >= max → exit.
		if n.MaxIters != nil && k >= *n.MaxIters {
			return OutcomeOK, nil
		}
	}
}

// failStep is the shared failure-emission helper. Appends a node.failed event
// AND returns the (outcome, err) tuple the caller propagates. The event is
// fsync'd via Log.Sync so a torn-tail post-halt still observes the failure
// record — node.failed IS the terminal record for the step; losing it means
// resume can't see what happened.
func failStep(log state.Log, path string, outcome Outcome, cause error) (Outcome, error) {
	errStr := ""
	if cause != nil {
		errStr = cause.Error()
	}
	data, err := json.Marshal(NodeFailedData{
		Outcome: string(outcome),
		Error:   errStr,
	})
	if err != nil {
		return "", fmt.Errorf("engine.Run: marshal node.failed at path %q: %w", path, err)
	}
	if err := log.Append(state.Event{
		Type: EventNodeFailed,
		Path: path,
		Data: data,
	}); err != nil {
		return "", fmt.Errorf("engine.Run: append node.failed at path %q: %w", path, err)
	}
	if err := log.Sync(); err != nil {
		return "", fmt.Errorf("engine.Run: sync log after node.failed at path %q: %w", path, err)
	}
	return outcome, cause
}

// drainTap consumes the per-attempt IOChunk channel from RunWithRetry's last
// attempt and writes each chunk to tap (prefixed with "[<stepID>] "). nil tap
// or nil channel are both no-ops. Blocks until the channel is closed —
// Phase 2's pre-closed fake channels make this finite.
//
// On tap-write error we KEEP DRAINING the channel (with tap disabled). A
// streaming Phase 4 backend whose writer goroutine fills the channel buffer
// would deadlock if the consumer stops reading.
//
// We track the "tap is still good" state in a LOCAL variable `w`, never
// mutating the `tap` parameter.
func drainTap(chunks <-chan container.IOChunk, stepID string, tap io.Writer) {
	if chunks == nil {
		return
	}
	w := tap
	for c := range chunks {
		if w == nil {
			continue // still drain; don't write
		}
		if _, err := fmt.Fprintf(w, "[%s] %s", stepID, c.Data); err != nil {
			w = nil
		}
	}
}
