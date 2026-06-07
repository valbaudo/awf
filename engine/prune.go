package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// pruneController applies a map prune policy as items commit. It is a pure
// decision object: callers report (itemIndex, score) via Report and read which
// items to cancel from the returned decision. The handler owns all ctx-cancel
// side effects — this type touches no ctx/log/backend/RunState (interpreter-is-
// the-only-state-writer is preserved: the controller is in-memory advisory).
// The stop_when scope is the single float best.score, so the controller needs
// only the policy and the running scores — NO *ir.Workflow, runtime path, or
// *RunState (those would be dead fields).
//
// Two policies (exactly one set, enforced by AWF1037):
//   - keep top(k): keep the k highest scorers; prune the rest, incrementally.
//   - stop_when: once the bounded bool expr over best.score is true, prune
//     everything still running (StopAll).
type pruneController struct {
	mu      sync.Mutex
	policy  *ir.Prune
	scores  map[int]float64 // itemIndex → committed score
	stopAll bool
}

func newPruneController(policy *ir.Prune) *pruneController {
	return &pruneController{
		policy: policy,
		scores: map[int]float64{},
	}
}

// pruneDecision is the result of reporting one item's score.
type pruneDecision struct {
	// PruneLosers are item indices that should now be cancelled (keep-top-k).
	PruneLosers []int
	// StopAll is true once stop_when fired — cancel EVERY still-running/queued
	// item (the caller passes those indices to its cancel routine).
	StopAll bool
}

// Report records item's score and returns the resulting prune decision. A
// non-numeric score is an error (the caller fails the whole map permanent, like
// a bad over). Idempotent per item (a re-report overwrites; resume safety).
func (p *pruneController) Report(item int, raw any) (pruneDecision, error) {
	score, err := asFloat(raw)
	if err != nil {
		return pruneDecision{}, fmt.Errorf("prune.score: item %d: %w", item, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scores[item] = score

	if p.policy.StopWhen != "" {
		ok, evalErr := p.evalStopWhen(score)
		if evalErr != nil {
			return pruneDecision{}, fmt.Errorf("prune.stop_when: %w", evalErr)
		}
		if ok {
			p.stopAll = true
			return pruneDecision{StopAll: true}, nil
		}
		return pruneDecision{}, nil
	}

	// keep top(k): if more than k items have committed a score, the lowest
	// (ties: highest index pruned first → lowest index wins) are losers. The
	// returned set is the FULL current loser set (recomputed each call); the
	// caller dedupes indices it has already cancelled.
	k := p.policy.Keep.K
	if len(p.scores) <= k {
		return pruneDecision{}, nil
	}
	losers := p.lowestBeyondK(k)
	return pruneDecision{PruneLosers: losers}, nil
}

// lowestBeyondK returns the item indices NOT in the current top-k by score.
// Deterministic: sort by (score desc, index asc); everything past position k is
// a loser. Recomputed each call; the caller dedupes already-cancelled indices.
func (p *pruneController) lowestBeyondK(k int) []int {
	type sc struct {
		idx   int
		score float64
	}
	all := make([]sc, 0, len(p.scores))
	for i, s := range p.scores {
		all = append(all, sc{i, s})
	}
	sort.Slice(all, func(a, b int) bool {
		if all[a].score != all[b].score {
			return all[a].score > all[b].score // higher score first
		}
		return all[a].idx < all[b].idx // tie: lower index survives
	})
	losers := make([]int, 0, len(all)-k)
	for i := k; i < len(all); i++ {
		losers = append(losers, all[i].idx)
	}
	sort.Ints(losers)
	return losers
}

// evalStopWhen evaluates the bounded bool expr over a scope exposing best.score
// (the max committed score so far, including this one). Reuses the existing
// template evaluator — no new language.
func (p *pruneController) evalStopWhen(latest float64) (bool, error) {
	best := latest
	for _, s := range p.scores {
		if s > best {
			best = s
		}
	}
	scope := newBestScope(best)
	return template.EvalBoolString(p.policy.StopWhen, scope)
}

// asFloat coerces a JSON-materialized score to float64. node.completed outputs
// fold via json.Unmarshal into map[string]any (engine/fold.go), so numbers
// arrive as float64; json.Number and int are accepted defensively.
func asFloat(raw any) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case json.Number:
		return v.Float64()
	case nil:
		return 0, fmt.Errorf("score is absent (nil)")
	default:
		return 0, fmt.Errorf("score is %T, want a number", raw)
	}
}

// bestScope is the minimal template.Scope for prune.stop_when: it resolves the
// single ref best.score to the running best, and rejects everything else
// (stop_when's grammar is best.score only, per the man page). Far lighter than
// engine.Scope, and it keeps stop_when from reaching arbitrary run state.
type bestScope struct{ best float64 }

func newBestScope(best float64) *bestScope { return &bestScope{best: best} }

// Resolve implements template.Scope. Resolves only the 2-segment ref best.score
// to the running best; anything else is AWF4002 (EvalCodeRefUnresolved).
func (s *bestScope) Resolve(ref *template.Ref) (any, error) {
	g := ref.Segments
	if len(g) == 2 && !g[0].IsIndex && g[0].Ident == "best" && !g[1].IsIndex && g[1].Ident == "score" {
		return s.best, nil
	}
	return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "prune.stop_when may only reference best.score")
}
