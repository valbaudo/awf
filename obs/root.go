package obs

import (
	"sort"
	"time"

	"github.com/valbaudo/awf/engine"
)

// buildRootSpan creates the run-root span (path ""). Its bounds are
// run.started → run.finished/cancelled; if neither terminal event exists the
// root is Pending (End = lastTS). awf.run.cost.usd is the sum of LEAF step
// costs only (!isScope, i.e. Scope==false), so synthesized scope spans never
// double-count (M1 + the parallel/map dedup case).
func buildRootSpan(byPath map[string]*Span, rs *engine.RunStartedData, start, end time.Time, finalized bool, lastTS time.Time, epoch uint32, outcome string) *Span {
	root := &Span{Path: "", Name: "run", Kind: "run", Scope: true, Attributes: map[string]any{}}
	root.Start = start
	if epoch > 0 {
		root.Attributes[AttrRunEpoch] = int64(epoch)
	}
	if finalized {
		root.End = end
		// R2: root status is a deterministic function of the run outcome — a
		// failure outcome (retryable_failure / permanent_failure) or a cancelled
		// run → Error; ok → Unset (OTel Ok is an unoverridable success assertion
		// we never make, and status is never rolled up from children).
		if outcome != "" && outcome != string(engine.OutcomeOK) {
			root.Status = StatusError
			root.StatusMsg = outcome
		}
	} else {
		root.End = lastTS
		root.Pending = true
		root.Attributes[AttrNodeOutcome] = outcomeIncomplete
	}
	if rs != nil {
		if rs.RunID != "" {
			root.Attributes[AttrRunID] = rs.RunID
		}
		if rs.WorkflowID != "" {
			root.Attributes[AttrWorkflowID] = rs.WorkflowID
		}
		if rs.WorkflowVersion != 0 {
			root.Attributes[AttrWorkflowVersion] = int64(rs.WorkflowVersion)
		}
		if rs.WorkflowDigest != "" {
			root.Attributes[AttrWorkflowDigest] = rs.WorkflowDigest
		}
	}
	// R1: sum leaf costs in a DETERMINISTIC (sorted-path) order — float64
	// addition is non-associative, so map-iteration order would make
	// awf.run.cost.usd non-deterministic and break byte-identical replay.
	if total, any := sumLeafCostsUSD(byPath); any {
		root.Attributes[AttrRunCostUSD] = total
	}
	return root
}

// sumLeafCostsUSD sums awf.cost.usd over leaf (non-scope) spans in sorted-path
// order, so the result is independent of map-iteration order (R1). float64
// addition isn't associative; a fixed summation order is what restores
// determinism. Cost stays float64 USD end-to-end (matches the awf.cost.usd
// contract + the adapter's reported total_cost_usd; integer micro-USD would
// ripple into agent.MetricCost — out of scope). Returns (0,false) if no leaf
// carries a cost.
func sumLeafCostsUSD(byPath map[string]*Span) (float64, bool) {
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var total float64
	hasCost := false
	for _, p := range paths {
		s := byPath[p]
		if p == "" || s.Scope {
			continue
		}
		if c, ok := s.Attributes[AttrCostUSD].(float64); ok {
			total += c
			hasCost = true
		}
	}
	return total, hasCost
}

// applyRunScopedAttrs stamps run-level identity onto every span: awf.run.id on
// all, and gen_ai.conversation.id + session.id on agent spans (run id is the
// conversation grouping key — spec D7).
func applyRunScopedAttrs(byPath map[string]*Span, rs *engine.RunStartedData) {
	if rs == nil || rs.RunID == "" {
		return
	}
	for path, s := range byPath {
		if path == "" {
			continue
		}
		s.Attributes[AttrRunID] = rs.RunID
		if s.Kind == "agent" {
			s.Attributes[AttrGenAIConversation] = rs.RunID
			s.Attributes[AttrSessionID] = rs.RunID
		}
	}
}
