package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/sync/semaphore"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

// runMap is the Map handler (Phase 3 spec §5.7 + design §E). Implements the
// minimum-runnable scope (design decision 4):
//
//   - Evaluate `over` to a typed []any (Design Q1: bare-ref restriction).
//   - Create N container instances via Backend.Create (one per item).
//   - Dispatch each item's body concurrently, capped at Concurrency.
//   - Wait for every item to terminate (no early termination per design §E
//     step 4).
//   - Tally success / failure against MinSuccess (default = all items).
//   - Commit one map.item{N, Status} event per terminated item.
//
// Per-item container provisioning routes the body's container lookup to the
// per-item handle via LocalDispatcher.WithItemHandle (Task 5). Backend access
// is via downcast dispatcher.(*LocalDispatcher) per Design Q2 — Phase 3 has
// one production Dispatcher; the gate test rigs don't exercise map.
//
// Resume:
//   - Re-evaluate `over` (deterministic against committed step outputs).
//   - For each item N: if map.item{N} is in MapItems, skip body re-exec
//     (already committed). Update ItemValue in-memory via UpdateMapItemValue.
//   - Else: dispatch body for that item (uncommitted frontier).
//
// Skip-inside-item: ends THAT item as item_passed (design §E step 5). The
// `node.skipped` event is appended for trace; the per-item commit then fires
// item_passed.
func runMap(
	ctx context.Context,
	n *ir.Map,
	mapPath string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	broker *signal.Broker,
) (Outcome, error) {
	return runMapWithContext(ctx, n, mapPath, interpreterContext{
		wf: wf, runstate: runstate, dispatcher: dispatcher, log: log, blobs: blobs, clk: clk, tap: tap, broker: broker,
	})
}

func runMapWithContext(
	ctx context.Context,
	n *ir.Map,
	mapPath string,
	ictx interpreterContext,
) (Outcome, error) {
	ld, ok := ictx.dispatcher.(*LocalDispatcher)
	if !ok {
		// Design Q2: Phase 3 production dispatcher is *LocalDispatcher; map
		// needs Backend access for per-item container provisioning. Test
		// rigs that don't pass *LocalDispatcher don't exercise map.
		return "", fmt.Errorf("engine.runMap: map at %q requires *LocalDispatcher for per-item container provisioning (got %T)", mapPath, ictx.dispatcher)
	}

	// 1. Evaluate `over`. Design Q1: bare reference; the resolved value must
	//    be a []any. Empty/nil → ok with zero items.
	overScope := ictx.scope(mapPath + ".over")
	overVal, err := evalOver(string(n.Over), overScope)
	if err != nil {
		return failStep(ictx.log, mapPath, OutcomePermanentFailure, fmt.Errorf("evaluate over: %w", err))
	}
	overArr, ok := overVal.([]any)
	if !ok {
		return failStep(ictx.log, mapPath, OutcomePermanentFailure,
			fmt.Errorf("`over` resolved to %T, want []any (spec §5.7 — `over` must be a typed array)", overVal))
	}
	if len(overArr) == 0 {
		return OutcomeOK, nil // no items, no work.
	}

	// 2. Resume reconciliation. Walk existing MapItems[mapPath]: record each
	//    N's Status in `committed`; re-fill ItemValue from overArr[N] (Design Q3);
	//    track maxCommittedN to detect non-deterministic `over` (H8).
	committed := map[int]string{} // N → Status, for items already in MapItems
	maxCommittedN := -1
	for _, mr := range ictx.runstate.LookupMapItems(mapPath) {
		committed[mr.N] = mr.Status
		if mr.N > maxCommittedN {
			maxCommittedN = mr.N
		}
		// Fill in ItemValue post-resume (Design Q3) iff still in-bounds.
		if mr.ItemValue == nil && mr.N < len(overArr) {
			ictx.runstate.UpdateMapItemValue(mapPath, mr.N, overArr[mr.N])
		}
	}
	// H8: fail-loud if over re-evaluated to fewer items than were committed.
	if maxCommittedN >= len(overArr) {
		return "", fmt.Errorf("engine.runMap: map %q: non-deterministic `over` — committed item N=%d exists but current over yields only %d items; `over` template depends on a value that changed across resume (CLAUDE.md determinism invariant)",
			mapPath, maxCommittedN, len(overArr))
	}

	// 3. Concurrency cap. semaphore.Weighted is ctx-respecting (H6) — Acquire
	//    returns ctx.Err on cancel without blocking forever.
	capSize := int64(n.Concurrency)
	if capSize < 1 {
		capSize = 1 // defense-in-depth; validator AWF1012 should have caught zero/negative
	}
	if capSize > int64(len(overArr)) {
		capSize = int64(len(overArr))
	}
	sem := semaphore.NewWeighted(capSize)

	// 3a. Slice-3.2 patterns: wrap log + tap for concurrent goroutines.
	wrappedLog := newSerializingLog(ictx.log)
	var wrappedTap io.Writer
	if ictx.tap != nil {
		wrappedTap = newSerializingWriter(ictx.tap)
	}

	// 3b. SP5 prune frontier. Build the controller + the in-flight cancel /
	//     pruned-set registry only when the map declares a prune: clause. nil for
	//     a plain map — every prune-specific branch below is guarded by `pr != nil`,
	//     so a non-prune map runs byte-identically.
	var pr *pruneRun
	if n.Prune != nil {
		stepID, ok := ir.LastStepID(n.Body)
		if !ok {
			// Should not happen — AWF5008 guarantees the last body node is a step.
			return failStep(ictx.log, mapPath, OutcomePermanentFailure,
				fmt.Errorf("engine.runMap: map %q declares prune: but the body's last node is not a step (AWF5008 should have caught)", mapPath))
		}
		pr = newPruneRun(n.Prune, mapPath, stepID)
	}

	statuses := make([]string, len(overArr)) // per-item terminal Status; pre-filled for already-committed items
	var statusErr error
	var statusErrMu sync.Mutex // protects statusErr
	var wg sync.WaitGroup

	for i := 0; i < len(overArr); i++ {
		i := i
		// Skip already-committed items (resume: replayed, not recomputed). A
		// folded item_pruned status lands here too — the prune decision is
		// replayed verbatim, never re-fed to the controller (resume safety).
		if status, ok := committed[i]; ok {
			statuses[i] = status
			continue
		}
		// Record a pending MapItemRecord BEFORE goroutine fires so the
		// body's templates can resolve <as>.<field> via LookupMapItems.
		ictx.runstate.RecordMapItem(mapPath, MapItemRecord{
			N:         i,
			ItemValue: overArr[i],
			Status:    "", // pending
		})

		wg.Add(1)
		go func() {
			defer wg.Done()

			// Per-item dispatch ctx. For a prune map this is a cancellable child
			// of the map ctx so a frontier loser can be cancelled INDIVIDUALLY
			// (the map's own ctx, and thus its siblings, is untouched). For a
			// plain map it is just the map ctx.
			itemCtx := ctx
			if pr != nil {
				var itemCancel context.CancelFunc
				itemCtx, itemCancel = context.WithCancel(ctx)
				defer itemCancel()
				pr.register(i, itemCancel)
				// Queued-loser short-circuit: a not-yet-started item already
				// marked pruned by an earlier commit never acquires a slot and
				// never runs its body. Its durable item_pruned commit lands in
				// the final pass after wg.Wait().
				if pr.isPruned(i) {
					return
				}
			}

			// ctx-respecting Acquire. On ctx cancel, returns immediately
			// without ever holding the semaphore slot.
			if err := sem.Acquire(itemCtx, 1); err != nil {
				// A prune cancel of THIS item's ctx is not an error — it is the
				// frontier discarding a loser; the final pass commits it pruned.
				if pr != nil && pr.isPruned(i) {
					return
				}
				statusErrMu.Lock()
				if statusErr == nil {
					statusErr = fmt.Errorf("engine.runMap: acquire semaphore for item-%d: %w", i, err)
				}
				statusErrMu.Unlock()
				return
			}
			defer sem.Release(1)

			// Re-check after acquiring: an item may have been pruned while it
			// waited for a slot.
			if pr != nil && pr.isPruned(i) {
				return
			}

			itemIctx := ictx
			itemIctx.log = wrappedLog
			itemIctx.tap = wrappedTap
			status, dispatchErr := dispatchItem(itemCtx, n, mapPath, i, pr, ld, itemIctx)
			statuses[i] = status
			if dispatchErr != nil {
				// A prune cancel unwinds the body via ctx; that is not an internal
				// error — the loser is already in the pruned set and the final pass
				// commits it pruned.
				if pr != nil && pr.isPruned(i) {
					return
				}
				statusErrMu.Lock()
				if statusErr == nil {
					statusErr = dispatchErr // first internal error wins
				}
				statusErrMu.Unlock()
				return
			}

			// SP5: a fresh-passing item reports its typed score to the frontier.
			// The controller's decision marks + cancels losers; the final pass
			// (after wg.Wait) commits the authoritative per-item status, so the
			// commit is race-free regardless of concurrent completion order.
			if pr != nil && status == ItemPassed {
				if dErr := pr.report(ictx.runstate, i); dErr != nil {
					statusErrMu.Lock()
					if statusErr == nil {
						statusErr = dErr
					}
					statusErrMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	// 3c. SP5 ATOMIC frontier commit. A prune disposition is a GLOBAL decision
	//     (keep: top(k) / stop_when) that is only valid once every item has
	//     reported and the frontier settled — so it is committed AFTER wg.Wait
	//     (committing inside a goroutine would race a later prune of an already-
	//     finished winner) AND all-or-nothing in ONE map.frontier event. A
	//     per-item commit loop could crash mid-pass and leave a PARTIAL frontier:
	//     resume would skip the committed survivors (their scores never re-enter a
	//     fresh controller) and re-derive the rest against an incomplete score set,
	//     yielding more survivors than keep: top(k) allows. One atomic event makes
	//     that impossible — a crash before the single append leaves zero committed
	//     dispositions, so resume re-runs the whole map from a clean slate (bodies
	//     replay from their own committed node.completed; the frontier is decided
	//     fresh, with no committed disposition to contradict). Each fresh item's
	//     authoritative status is recorded here: item_pruned if the frontier
	//     discarded it (incl. a queued loser whose body never ran), else its body
	//     status. On a pure replay (every item already committed) there is nothing
	//     to decide — the disposition is already durable, so skip the commit.
	if pr != nil && statusErr == nil {
		var fresh []MapItemData
		for i := 0; i < len(overArr); i++ {
			if _, done := committed[i]; done {
				continue // replayed from a prior run; the frontier is already durable
			}
			final := statuses[i]
			if pr.isPruned(i) {
				final = ItemPruned
			}
			statuses[i] = final
			fresh = append(fresh, MapItemData{N: i, Status: final})
		}
		if len(fresh) > 0 {
			if cErr := commitMapFrontier(ictx.log, ictx.runstate, mapPath, fresh); cErr != nil {
				return "", cErr
			}
		}
	}

	// 4. Internal dispatch error. A per-item runtime-image RENDER fault (bad
	//    image: template or an empty-rendered image) OR a per-item Create CONFIG
	//    fault (the backend rejected an invalid per-element spec — bad resources,
	//    a host config the daemon refuses) is a deterministic DEFINITION error:
	//    fail the whole map LOUD as permanent_failure (like an unrenderable
	//    `over`), never laundered into a tolerated item_failed. Other internal
	//    errors (static Create failure, log append, ctx cancel) propagate raw.
	if statusErr != nil {
		var rie *renderImageError
		var cce *createConfigError
		if errors.As(statusErr, &rie) || errors.As(statusErr, &cce) {
			return failStep(ictx.log, mapPath, OutcomePermanentFailure, statusErr)
		}
		return "", statusErr
	}

	// 5. Tally per MinSuccess (the success gate). For a non-reduce map this is
	//    the terminal verdict; for a reduce map the min_success gate is bypassed
	//    (a quorum reduce generalizes it — quorum is the success threshold), so a
	//    reduce node's verdict is the reducer's outcome (step 6).
	// Pruned items (SP5) are removed from BOTH the numerator (tallyResults
	// already ignores them) AND the denominator: a pruned item is a deliberate
	// cancellation by the frontier, not a baseline expectation, so min_success
	// is measured against the NON-pruned set. effectiveTotal shrinks by the
	// pruned count; defaultMinSuccess(n, 0) returns 0 for an all-pruned map,
	// so an entirely-pruned frontier (e.g. stop_when that fired immediately) is
	// a success, not a failure.
	pass, fail := tallyResults(statuses)
	pruned := countPruned(statuses)
	effectiveTotal := len(overArr) - pruned
	minSuccess := defaultMinSuccess(n, effectiveTotal)
	if n.Reduce == nil {
		if int64(pass) >= minSuccess {
			return OutcomeOK, nil
		}
		return OutcomeRetryableFailure, fmt.Errorf("engine.runMap: map %q: %d items passed, %d failed, %d pruned; min_success requires %d of %d non-pruned",
			mapPath, pass, fail, pruned, minSuccess, effectiveTotal)
	}

	// 6. Fan-IN (C2a): collapse the surviving branches via the reduce: clause
	//    (engine/reduce.go owns the fan-in, beside runReduce). effectiveTotal
	//    (= len(overArr) - pruned) is the quorum cohort — a pruned item is a
	//    deliberate frontier cancellation, removed from the denominator exactly as
	//    it is from min_success above; a mechanically-failed branch still counts
	//    (absent from branches → a non-agreeing vote), so passing len(overArr)
	//    would demand agreement from items the frontier deliberately discarded.
	return runMapReduce(ctx, n, mapPath, effectiveTotal, ictx.wf, ictx.moduleID, ictx.runstate, ld, ictx.log, ictx.blobs, ictx.clk, ictx.tap)
}

// renderImageError marks a per-item map.image render fault (the image: template
// failed to substitute, or rendered to an empty/whitespace string) — a
// DETERMINISTIC definition/data error, not per-item infra. dispatchItem returns
// it so runMap converts it to a hard permanent_failure for the whole map (fail
// loud, like an unrenderable `over`) instead of laundering a template bug into a
// tolerated item_failed under min_success. (image_unavailable — a non-empty ref
// that won't boot — stays a tolerated per-item failure.)
type renderImageError struct {
	itemN int
	err   error
}

func (e *renderImageError) Error() string {
	return fmt.Sprintf("map item-%d: runtime image: failed to render: %v", e.itemN, e.err)
}

func (e *renderImageError) Unwrap() error { return e.err }

// createConfigError marks a per-item Backend.Create failure that is NOT a
// tolerated image-availability fault (container.ImageUnavailableError) — the
// backend rejected the per-element SPEC itself (a malformed resources:, a host
// config the daemon refused). Like renderImageError it is a DETERMINISTIC
// definition error: dispatchItem returns it so runMap fails the WHOLE map as
// permanent_failure, never laundering an author's broken definition into a
// tolerated item_failed under min_success (the bug that hid `mem: 4Gi`). A
// genuine "couldn't pull/boot this image" failure stays a tolerated per-item
// ReasonImageUnavailable instead.
type createConfigError struct {
	itemN int
	err   error
}

func (e *createConfigError) Error() string {
	return fmt.Sprintf("map item-%d: container Create rejected the per-element spec (definition error): %v", e.itemN, e.err)
}

func (e *createConfigError) Unwrap() error { return e.err }

// dispatchItem runs body for one item. Provisions a per-item container handle,
// builds a per-item dispatcher, recursively walks body, commits the map.item
// event, and Destroys the container.
//
// Returns the per-item terminal status (ItemPassed / ItemFailed) AND a
// separate error for INTERNAL failures (Backend.Create / Destroy / log
// append) — those propagate to runMap which returns ("", err) to the
// interpreter.
//
// SP5: when pr != nil (a prune map) the map.item commit is DEFERRED to runMap's
// final pass — dispatchItem returns the body status without committing, so the
// authoritative pruned-vs-passed decision can be made after the frontier
// settles (commit-after-join is race-free). A plain map (pr == nil) commits
// internally, byte-identically to pre-SP5.
func dispatchItem(
	ctx context.Context,
	n *ir.Map,
	mapPath string,
	itemN int,
	pr *pruneRun,
	ld *LocalDispatcher,
	ictx interpreterContext,
) (string, error) {
	itemPath := ItemPath(mapPath, itemN) // "map[0].item-3"

	spec := ContainerSpecFor(ictx.wf, ld.ComposeFiles, n.Container)
	// Per-item containers MUST have distinct names: the docker backend derives the
	// container name from spec.Name, so every item sharing n.Container would collide
	// on a concurrent Create (the fake ignores Name, so this path went untested until
	// the first real docker map run). Suffix the item index for uniqueness; the body
	// still looks the handle up by n.Container via WithItemHandle below, so this only
	// changes the backend-level container name.
	spec.Name = fmt.Sprintf("%s-item-%d", n.Container, itemN)

	// P6a: runtime-resolved per-element image. Render map.image against THIS
	// item's scope — the pending MapItemRecord (recorded by runMap before this
	// goroutine fired) lets {{ <as>.field }} resolve — then boot that image.
	// A render fault (bad template or an empty result) is a deterministic
	// definition/data error: return a *renderImageError sentinel so runMap fails
	// the WHOLE map as permanent_failure (fail loud, like an unrenderable `over`).
	// An unavailable image (a non-empty ref that won't boot) is a per-item
	// tolerated failure only (item_failed + ReasonImageUnavailable, counted
	// against min_success). A STATIC container's Create failure stays an internal
	// hard error (below) — that is infra for a definition-pinned image.
	if n.Image != "" {
		rendered, rErr := template.Substitute(string(n.Image), ictx.scope(itemPath))
		if rErr != nil {
			// A render fault is a deterministic definition/data error → fail the
			// whole map (runMap converts this sentinel to permanent_failure).
			return "", &renderImageError{itemN: itemN, err: rErr}
		}
		if strings.TrimSpace(rendered) == "" {
			return "", &renderImageError{itemN: itemN, err: fmt.Errorf("map.image %q rendered to an empty string", n.Image)}
		}
		spec.Image = rendered
		// Runtime-resolved image: it was learned just now, so it cannot have
		// been pre-provisioned — the backend must pull it (and, for a real
		// backend, require a digest pin + capture the booted digest). The fake
		// ignores the flag; its digest comes from the programmed table.
		spec.PullIfAbsent = true
	}

	itemHandle, err := ld.Backend.Create(ctx, spec)
	if err != nil {
		if n.Image != "" {
			// A context cancellation/timeout (possibly wrapped in an
			// ImageUnavailableError by the backend) is a transient control signal —
			// NOT a tolerated availability failure NOR a definition error. Propagate
			// it raw, like the static path below, so a cancelled run stays resumable
			// (its frontier uncommitted) instead of committing the map as a
			// permanent_failure that resume would never retry.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", err
			}
			// ONLY a container.ImageUnavailableError — a valid image that couldn't be
			// pulled/booted, or an untrusted non-pinned rendered ref — is a tolerated
			// per-item infra failure. For a prune map the commit is deferred to the
			// final pass (image_unavailable reason is recorded only on the non-prune
			// commit path).
			var iu *container.ImageUnavailableError
			if errors.As(err, &iu) {
				if pr != nil {
					return ItemFailed, nil
				}
				return commitMapItem(ictx.log, ictx.runstate, mapPath, itemN, ItemFailed, "", ReasonImageUnavailable)
			}
			// Any OTHER Create error is a deterministic definition error (an invalid
			// per-element spec — bad resources, a host config the daemon rejects):
			// fail the WHOLE map LOUD as permanent_failure (like a render fault),
			// never laundered into a tolerated item_failed under min_success. This
			// return is intentionally pr-agnostic — a prune map fails just as loud;
			// only the TOLERATED path above is prune-gated.
			return "", &createConfigError{itemN: itemN, err: err}
		}
		return "", fmt.Errorf("create item-%d container: %w", itemN, err)
	}
	imageDigest := itemHandle.ResolvedImageDigest
	defer func() {
		// Destroy errors at item end are not fatal — the item is terminated and
		// Destroy is cleanup. context.Background() so a parent-ctx cancel still
		// tears the container down. L14 (Phase 4): the Docker backend should wrap
		// this with context.WithTimeout(context.Background(), teardownGrace) to
		// avoid an indefinite hang on shutdown (Fake.Destroy is instant).
		_ = ld.Backend.Destroy(context.Background(), itemHandle)
	}()

	// Per-item dispatcher with the override.
	perItemDispatcher := ld.WithItemHandle(n.Container, itemHandle)
	itemCtx := ictx
	itemCtx.dispatcher = perItemDispatcher

	// Walk body.
	bodyOC, bodyErr := interpNodes(ctx, n.Body, itemPath, itemCtx)

	status := ItemPassed // default optimistic; revised below
	var su *SkipUnwind
	if errors.As(bodyErr, &su) {
		// Skip ends the item as ok (design §E step 5). Record node.skipped
		// for trace; status stays ItemPassed.
		if appErr := appendNodeSkipped(ictx.log, itemPath, su.Reason); appErr != nil {
			return "", fmt.Errorf("append node.skipped for item-%d: %w", itemN, appErr)
		}
	} else if bodyErr != nil || bodyOC != OutcomeOK {
		// Body failed mechanically (any non-skip error) → item_failed. For a prune
		// map this includes a frontier cancel (ctx unwind of an in-flight loser);
		// runMap's final pass overrides it with item_pruned for any pruned[i].
		status = ItemFailed
	}

	// SP5: defer the map.item commit to runMap's final pass for a prune map.
	if pr != nil {
		return status, nil
	}
	return commitMapItem(ictx.log, ictx.runstate, mapPath, itemN, status, imageDigest, "")
}

// commitMapItem appends the map.item commit (with the optional captured runtime
// image digest and failure reason), fsyncs, mirrors the in-memory status, and
// returns the item's terminal status. Extracted (P6a) so the normal end and the
// per-item image-failure paths share one commit point and preserve commit-
// atomicity (digest+reason are in the payload BEFORE Append+Sync).
func commitMapItem(log state.Log, runstate *RunState, mapPath string, itemN int, status, imageDigest, reason string) (string, error) {
	data, mErr := json.Marshal(MapItemData{N: itemN, Status: status, ImageDigest: imageDigest, Reason: reason})
	if mErr != nil {
		return "", fmt.Errorf("marshal map.item for item-%d: %w", itemN, mErr)
	}
	if err := log.Append(state.Event{
		Type: EventMapItem,
		Path: mapPath,
		Data: data,
	}); err != nil {
		return "", fmt.Errorf("append map.item for item-%d: %w", itemN, err)
	}
	if err := log.Sync(); err != nil {
		return "", fmt.Errorf("sync log after map.item for item-%d: %w", itemN, err)
	}
	updateMapItemStatus(runstate, mapPath, itemN, status)
	return status, nil
}

// commitMapFrontier appends a prune map's full per-item disposition as ONE
// atomic map.frontier event (Append+Sync), then mirrors each item's status into
// RunState.MapItems. The whole frontier rides a single journal entry so a crash
// can never leave a partial disposition that resume would re-derive against an
// incomplete score set (resume safety — see EventMapFrontier). items is the
// fresh (not-already-committed) disposition; an empty slice never reaches here
// (the caller skips a pure replay).
func commitMapFrontier(log state.Log, runstate *RunState, mapPath string, items []MapItemData) error {
	data, mErr := json.Marshal(MapFrontierData{Items: items})
	if mErr != nil {
		return fmt.Errorf("marshal map.frontier for %q: %w", mapPath, mErr)
	}
	if err := log.Append(state.Event{
		Type: EventMapFrontier,
		Path: mapPath,
		Data: data,
	}); err != nil {
		return fmt.Errorf("append map.frontier for %q: %w", mapPath, err)
	}
	if err := log.Sync(); err != nil {
		return fmt.Errorf("sync log after map.frontier for %q: %w", mapPath, err)
	}
	for _, it := range items {
		updateMapItemStatus(runstate, mapPath, it.N, it.Status)
	}
	return nil
}

// updateMapItemStatus walks RunState.MapItems[mapPath] under the RunState
// mutex and sets the matching N's Status. No-op if no match (defense-in-depth;
// the matching pending record was inserted by runMap before the goroutine
// fired).
func updateMapItemStatus(rs *RunState, mapPath string, n int, status string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	items := rs.MapItems[mapPath]
	for i, mr := range items {
		if mr.N == n {
			items[i].Status = status
			return
		}
	}
}

// evalOver evaluates `over` per Design Q1: parse as envelope, unwrap, expect a
// bare *template.Ref AST, call scope.Resolve. Compound expressions are rejected.
//
// Returns the typed value (typically []any) or an error.
func evalOver(src string, scope template.Scope) (any, error) {
	if src == "" {
		return nil, fmt.Errorf("`over` is empty (AWF1012 should have caught)")
	}
	inner := template.UnwrapEnvelope(src)
	e, err := template.ParseExpr(inner)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	ref, ok := e.(*template.Ref)
	if !ok {
		// Design Q1: bare reference only for slice 3.4 minimum.
		return nil, fmt.Errorf("`over` must be a single reference (got compound %T) — Phase 3 slice 3.4 minimum", e)
	}
	return scope.Resolve(ref)
}

// tallyResults counts ItemPassed vs other statuses in the per-item slice.
// Skipped slots (empty status) are NOT counted — they were committed in a
// prior run and folded back, so their statuses are non-empty.
func tallyResults(statuses []string) (pass, fail int) {
	for _, s := range statuses {
		switch s {
		case ItemPassed:
			pass++
		case ItemFailed:
			fail++
		case ItemPruned:
			// Pruned items count as neither pass nor fail (SP5): removed from
			// both the numerator and the min_success denominator (Task 4).
		case "":
			// Should not happen post-tally — defense-in-depth.
		}
	}
	return pass, fail
}

// countPruned counts ItemPruned statuses in the per-item slice (SP5). Used to
// shrink the min_success denominator: pruned items are neither numerator nor
// baseline.
func countPruned(statuses []string) int {
	n := 0
	for _, s := range statuses {
		if s == ItemPruned {
			n++
		}
	}
	return n
}

// defaultMinSuccess returns the effective MinSuccess for the map: nil → all items
// required, else the map's min_success Ratio interpreted by ratioThreshold.
func defaultMinSuccess(n *ir.Map, total int) int64 {
	return ratioThreshold(n.MinSuccess, total)
}

// ratioThreshold interprets a min_success / quorum Ratio against a total. nil →
// all (total). Int → that count. Fraction → ceil(fraction * total) for a fraction
// in (0, 1]. Shared by defaultMinSuccess (min_success) and quorumThreshold
// (quorum, engine/reduce.go) so the two thresholds parse by one rule.
//
// Phase 3 minimum: simple parsing; floats are rounded UP to the nearest int per
// the spec's "at least this many" semantics.
//
// Design Q7 (M10): AWF1012 does NOT validate the shape. On unparseable input
// (e.g. "abc"), this falls through to total — fail-safe (treat as "all
// required," the most conservative interpretation). The author sees a clear
// "requires N" error from runMap when the first item fails.
//
// L13 EDGE CASES:
//   - Negative int → total (degenerate; fails-safe).
//   - Int > total → clamped to total.
//   - Fraction <= 0 → total (degenerate; "succeed even if everything fails" is
//     not accepted — express "no items required" by removing the map).
//   - Fraction > 1 → clamped to total.
//   - Unparseable string (per Q7) → total.
func ratioThreshold(r *ir.Ratio, total int) int64 {
	if r == nil {
		return int64(total)
	}
	// Try int first.
	if i, err := r.Int64(); err == nil {
		if i < 0 || i > int64(total) {
			return int64(total)
		}
		return i
	}
	// Try float (fraction in (0, 1]).
	if f, err := r.Float64(); err == nil {
		if f <= 0 || f > 1 {
			return int64(total)
		}
		// Ceil.
		needed := int64(f * float64(total))
		if float64(needed) < f*float64(total) {
			needed++
		}
		if needed < 1 {
			needed = 1
		}
		return needed
	}
	// Unparseable — conservative.
	return int64(total)
}
