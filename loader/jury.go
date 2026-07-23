package loader

import (
	"github.com/valbaudo/awf/ir"
)

// juryBinding is the reserved `as:` name the desugared map binds each juror's
// with:-patch under. Chosen to be unlikely to collide with an author's own
// binding.
const juryBinding = "__juror"

// desugarJury lowers every AgentStep carrying a jury: block into a map+quorum,
// in place, over wf.Graph and every nested NodeList. Runs in loader.Load BEFORE
// ComputeDigest/ir.Validate so the sugared and hand-written forms normalize to
// byte-identical IR. Idempotent on a jury-free workflow.
func desugarJury(wf *ir.Workflow) {
	if wf == nil {
		return
	}
	rewriteJuryList(wf.Graph)
}

// rewriteJuryList replaces every AgentStep with a non-nil Jury by its desugared
// *ir.Map, recursing into every nested NodeList. Mirrors ir.WalkNodes's child-list
// coverage exactly, but — unlike WalkNodes, which is read-only — must write back
// into the containing slice, so it cannot be built on top of it.
func rewriteJuryList(list ir.NodeList) {
	for i, n := range list {
		switch v := n.(type) {
		case *ir.AgentStep:
			if v.Jury != nil {
				list[i] = juryToMap(v)
			}
		case *ir.If:
			rewriteJuryList(v.Then)
			rewriteJuryList(v.Else)
		case *ir.Loop:
			rewriteJuryList(v.Body)
		case *ir.Try:
			rewriteJuryList(v.Do)
			rewriteJuryList(v.Catch)
			rewriteJuryList(v.Finally)
		case *ir.Parallel:
			rewriteJuryList(v.Children)
		case *ir.Gate:
			rewriteJuryList(v.Generate)
			rewriteJuryList(v.Evaluate)
		case *ir.Map:
			rewriteJuryList(v.Body)
		case *ir.Compose:
			rewriteJuryList(v.Body)
		}
		// CodeStep/SignalStep/CallStep/Skip/React: no jury, no child NodeList.
	}
}

// juryToMap builds the map+quorum a jury: lowers to. The BODY step keeps the
// original id, uses, container, output_schema; each key that appears in any over:
// item is replaced in the body step's with: by a {{ <binding>.<key> }} template,
// and the over: patches become the map's literal OverItems.
//
// The MAP ITSELF IS ID-LESS. A jury verdict is addressed positionally via
// evaluate.<field> (it terminates a gate.evaluate — enforced by AWF1072), never
// by step.<map-id>. A NAMED map under a gate trips AWF5011 (per-attempt
// multiplication makes step.<id> ambiguous); dropping the id sidesteps it with no
// loss, since the id would be unreferenceable here anyway. The reduce commits at
// the map's PATH (bare map[i]), not its id, so the verdict still materializes.
func juryToMap(step *ir.AgentStep) *ir.Map {
	j := step.Jury

	// Collect the union of varied keys across over: items (validation guarantees
	// the key sets are uniform, so the union == any single item's key set).
	varied := map[string]struct{}{}
	for _, item := range j.Over {
		for k := range item {
			varied[k] = struct{}{}
		}
	}

	// Body step: a copy of the original with Jury stripped and varied keys
	// templated.
	body := *step
	body.Jury = nil
	newWith := ir.RawConfig{}
	for k, val := range step.With {
		newWith[k] = val
	}
	for k := range varied {
		// Plain string, not ir.Template: engine/agent_step.go's substituteRawConfig
		// type-asserts RawConfig string values as `string` (v.(string)) before
		// substituting — a decoded with: string is a plain Go string, never
		// ir.Template — so the templated value must match that representation to
		// be byte-identical to a hand-written with: key holding the same template.
		newWith[k] = "{{ " + juryBinding + "." + k + " }}"
	}
	body.With = newWith

	// OverItems is []any of the literal patches (map[string]any per item).
	overItems := make([]any, 0, len(j.Over))
	for _, item := range j.Over {
		overItems = append(overItems, item)
	}

	return &ir.Map{
		// NO ID — see the doc comment above. body.ID (== the original step id) is
		// retained on the inner step; the map aggregate is anonymous.
		As:        juryBinding,
		Container: step.Container,
		OverItems: overItems,
		Body:      ir.NodeList{&body},
		Reduce:    &ir.Reduce{Quorum: j.Quorum, Field: j.Field},
	}
}
