package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/sync/semaphore"

	"github.com/valbaudo/awf/clock"
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
	ld, ok := dispatcher.(*LocalDispatcher)
	if !ok {
		// Design Q2: Phase 3 production dispatcher is *LocalDispatcher; map
		// needs Backend access for per-item container provisioning. Test
		// rigs that don't pass *LocalDispatcher don't exercise map.
		return "", fmt.Errorf("engine.runMap: map at %q requires *LocalDispatcher for per-item container provisioning (got %T)", mapPath, dispatcher)
	}

	// 1. Evaluate `over`. Design Q1: bare reference; the resolved value must
	//    be a []any. Empty/nil → ok with zero items.
	overScope := NewScope(runstate, wf, mapPath+".over")
	overVal, err := evalOver(string(n.Over), overScope)
	if err != nil {
		return failStep(log, mapPath, OutcomePermanentFailure, fmt.Errorf("evaluate over: %w", err))
	}
	overArr, ok := overVal.([]any)
	if !ok {
		return failStep(log, mapPath, OutcomePermanentFailure,
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
	for _, mr := range runstate.LookupMapItems(mapPath) {
		committed[mr.N] = mr.Status
		if mr.N > maxCommittedN {
			maxCommittedN = mr.N
		}
		// Fill in ItemValue post-resume (Design Q3) iff still in-bounds.
		if mr.ItemValue == nil && mr.N < len(overArr) {
			runstate.UpdateMapItemValue(mapPath, mr.N, overArr[mr.N])
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
	wrappedLog := newSerializingLog(log)
	var wrappedTap io.Writer
	if tap != nil {
		wrappedTap = newSerializingWriter(tap)
	}

	statuses := make([]string, len(overArr)) // per-item terminal Status; pre-filled for already-committed items
	var statusErr error
	var statusErrMu sync.Mutex // protects statusErr
	var wg sync.WaitGroup

	for i := 0; i < len(overArr); i++ {
		i := i
		// Skip already-committed items (resume: replayed, not recomputed).
		if status, ok := committed[i]; ok {
			statuses[i] = status
			continue
		}
		// Record a pending MapItemRecord BEFORE goroutine fires so the
		// body's templates can resolve <as>.<field> via LookupMapItems.
		runstate.RecordMapItem(mapPath, MapItemRecord{
			N:         i,
			ItemValue: overArr[i],
			Status:    "", // pending
		})

		wg.Add(1)
		go func() {
			defer wg.Done()
			// ctx-respecting Acquire. On ctx cancel, returns immediately
			// without ever holding the semaphore slot.
			if err := sem.Acquire(ctx, 1); err != nil {
				statusErrMu.Lock()
				if statusErr == nil {
					statusErr = fmt.Errorf("engine.runMap: acquire semaphore for item-%d: %w", i, err)
				}
				statusErrMu.Unlock()
				return
			}
			defer sem.Release(1)

			status, dispatchErr := dispatchItem(ctx, n, mapPath, i, wf, runstate, ld, wrappedLog, blobs, clk, wrappedTap, broker)
			statuses[i] = status
			if dispatchErr != nil {
				statusErrMu.Lock()
				if statusErr == nil {
					statusErr = dispatchErr // first internal error wins
				}
				statusErrMu.Unlock()
			}
		}()
	}
	wg.Wait()

	// 4. Internal dispatch error (Backend.Create failure, log append failure,
	//    ctx cancel) is a hard error — distinct from a body-failure outcome.
	if statusErr != nil {
		return "", statusErr
	}

	// 5. Tally per MinSuccess.
	pass, fail := tallyResults(statuses)
	minSuccess := defaultMinSuccess(n, len(overArr))
	if int64(pass) >= minSuccess {
		return OutcomeOK, nil
	}
	return OutcomeRetryableFailure, fmt.Errorf("engine.runMap: map %q: %d items passed, %d failed; min_success requires %d",
		mapPath, pass, fail, minSuccess)
}

// dispatchItem runs body for one item. Provisions a per-item container handle,
// builds a per-item dispatcher, recursively walks body, commits the map.item
// event, and Destroys the container.
//
// Returns the per-item terminal status (ItemPassed / ItemFailed) AND a
// separate error for INTERNAL failures (Backend.Create / Destroy / log
// append) — those propagate to runMap which returns ("", err) to the
// interpreter.
func dispatchItem(
	ctx context.Context,
	n *ir.Map,
	mapPath string,
	itemN int,
	wf *ir.Workflow,
	runstate *RunState,
	ld *LocalDispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	broker *signal.Broker,
) (string, error) {
	itemPath := ItemPath(mapPath, itemN) // "map[0].item-3"

	spec := ContainerSpecFor(wf, ld.ComposeFiles, n.Container)

	// P6a: runtime-resolved per-element image. Render map.image against THIS
	// item's scope — the pending MapItemRecord (recorded by runMap before this
	// goroutine fired) lets {{ <as>.field }} resolve — then boot that image. A
	// per-item image problem (render failure, or an unavailable image) fails
	// THIS item only (item_failed + a machine-readable reason, counted against
	// min_success), never the whole map. A STATIC container's Create failure
	// stays an internal hard error (below) — that is infra for a definition-
	// pinned image, not a per-item result.
	if n.Image != "" {
		rendered, rErr := template.Substitute(string(n.Image), NewScope(runstate, wf, itemPath))
		if rErr != nil {
			return commitMapItem(log, runstate, mapPath, itemN, ItemFailed, "", "image_render_failed")
		}
		spec.Image = rendered
	}

	itemHandle, err := ld.Backend.Create(ctx, spec)
	if err != nil {
		if n.Image != "" {
			return commitMapItem(log, runstate, mapPath, itemN, ItemFailed, "", "image_unavailable")
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

	// Walk body.
	bodyOC, bodyErr := interpNodes(ctx, n.Body, itemPath, wf, runstate, perItemDispatcher, log, blobs, clk, tap, broker)

	status := ItemPassed // default optimistic; revised below
	var su *SkipUnwind
	if errors.As(bodyErr, &su) {
		// Skip ends the item as ok (design §E step 5). Record node.skipped
		// for trace; status stays ItemPassed.
		if appErr := appendNodeSkipped(log, itemPath, su.Reason); appErr != nil {
			return "", fmt.Errorf("append node.skipped for item-%d: %w", itemN, appErr)
		}
	} else if bodyErr != nil || bodyOC != OutcomeOK {
		// Body failed mechanically (any non-skip error) → item_failed.
		status = ItemFailed
	}

	return commitMapItem(log, runstate, mapPath, itemN, status, imageDigest, "")
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
		case "":
			// Should not happen post-tally — defense-in-depth.
		}
	}
	return pass, fail
}

// defaultMinSuccess returns the effective MinSuccess for the map. nil → all
// items required. Int → that count. Fraction → ceil(fraction * total) where
// the fraction is in (0, 1].
//
// Phase 3 minimum: simple parsing; floats are rounded UP to the nearest int
// per the spec's "at least this many" semantics.
//
// Design Q7 (M10): AWF1012 does NOT validate min_success shape. On unparseable
// input (e.g. min_success: "abc"), this function falls through to total —
// fail-safe (treat as "all required," the most conservative interpretation).
// The author sees a clear "min_success requires N" error from runMap when the
// first item fails. Future slice may add explicit shape validation per spec
// §11 if real-world workflows hit this footgun.
//
// L13 EDGE CASES:
//   - Negative int → treated as total (degenerate; fails-safe).
//   - Int > total → clamped to total.
//   - Fraction == 0 OR <= 0 → treated as total (degenerate; the author probably
//     meant "no items required" but that's better expressed by removing the
//     map or `concurrency: 0`; we don't accept "succeed even if everything
//     fails" semantics).
//   - Fraction > 1 → clamped to total.
//   - Unparseable string (per Q7) → total.
func defaultMinSuccess(n *ir.Map, total int) int64 {
	if n.MinSuccess == nil {
		return int64(total)
	}
	// Try int first.
	if i, err := n.MinSuccess.Int64(); err == nil {
		if i < 0 {
			return int64(total)
		}
		if i > int64(total) {
			return int64(total)
		}
		return i
	}
	// Try float (fraction in (0, 1]).
	if f, err := n.MinSuccess.Float64(); err == nil {
		if f <= 0 {
			return int64(total)
		}
		if f > 1 {
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
