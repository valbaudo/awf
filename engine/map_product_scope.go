package engine

import (
	"sort"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

type mapProduct struct {
	mapPath             string
	reduce              bool
	finalBodyStaticPath string
}

func mapProductIndex(wf *ir.Workflow) map[string]mapProduct {
	out := map[string]mapProduct{}
	if wf == nil {
		return out
	}
	ir.WalkNodes(wf.Graph, "", func(n ir.Node, path string) {
		m, ok := n.(*ir.Map)
		if !ok || m.ID == "" {
			return
		}
		if !ir.MapProductShape(path) {
			return
		}
		if m.Reduce != nil {
			out[m.ID] = mapProduct{mapPath: path, reduce: true}
			return
		}
		suffix, _, ok := ir.MapCompactProducer(m)
		if !ok {
			return
		}
		out[m.ID] = mapProduct{
			mapPath:             path,
			finalBodyStaticPath: appendSeg(appendSeg(path, "body"), suffix),
		}
	})
	return out
}

func (s *Scope) resolveMapProduct(id string, mp mapProduct, ref *template.Ref) (any, error) {
	if mp.reduce {
		nr, ok := s.rs.LookupCompleted(mp.mapPath)
		if !ok {
			return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "map product %q not yet committed (map path %q)", id, mp.mapPath)
		}
		if len(ref.Segments) == 2 {
			return nr.Outputs, nil
		}
		if err := mustIdent(ref.Segments[2], "map product field"); err != nil {
			return nil, err
		}
		if nr.Outputs == nil {
			return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "field %q: map product has no typed outputs", ref.Segments[2].Ident)
		}
		return descendPath(nr.Outputs, ref.Segments[2:], "step."+id+".")
	}
	agg, isAgg, err := s.aggregateMapOutputs(mp.finalBodyStaticPath, ref)
	if err != nil {
		return nil, err
	}
	if !isAgg {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "map product %q is only referenceable outside its producing map", id)
	}
	return agg, nil
}

// aggregateMapOutputs resolves step.<id>[.<field>...] when the producer is in the
// v1 single-map shape and the ref site is OUTSIDE that map: returns the index-
// ordered, compact []any of committed per-item outputs (whole output for a 2-seg
// ref, the descended field otherwise). isAgg=false → not an aggregate; caller falls
// through to normal resolution.
//
// NOTE: per-item descendPath errors are returned WITHOUT the step-context wrapper
// (they are item-specific; the validator already guarantees the field exists, so
// this path is defensive).
func (s *Scope) aggregateMapOutputs(staticPath string, ref *template.Ref) (any, bool, error) {
	mapStatic, suffix, ok := ir.SingleMapBodyShape(staticPath)
	if !ok {
		return nil, false, nil
	}
	if _, inside, err := s.instanceFromCtx(mapStatic, itemSep); err != nil {
		return nil, false, err
	} else if inside {
		return nil, false, nil // same-item ref → normal resolution
	}
	idSeg := ref.Segments[1]
	// C2a (reduce): if the enclosing map declared a reduce:, its reduced output
	// committed at the map's OWN path REPLACES the per-item aggregate. Prefer it.
	// Safe because a non-reduce map NEVER commits a node.completed at the bare
	// map path (only per-item ItemPaths + map.item events), so this misses and
	// the array path below runs unchanged.
	if nr, ok := s.rs.LookupCompleted(mapStatic); ok {
		if len(ref.Segments) == 2 {
			return nr.Outputs, true, nil
		}
		val, err := descendPath(nr.Outputs, ref.Segments[2:], "step."+idSeg.Ident+".")
		if err != nil {
			return nil, true, err
		}
		return val, true, nil
	}
	items := s.rs.LookupMapItems(mapStatic) // shallow copy — safe to sort in place
	sort.Slice(items, func(i, j int) bool { return items[i].N < items[j].N })
	out := []any{}
	for _, mr := range items {
		if mr.Status == ItemPruned {
			// A pruned item is a deliberate frontier cancellation, not a result, so
			// it must not appear in the cross-map aggregate even though its body
			// committed typed output before being pruned. (A mechanically-FAILED
			// item is already compacted out below by the committed-output miss, so
			// ItemPruned is the only status needing an explicit skip — this leaves
			// the same survivors collectReduceBranches feeds a reduce: fold.)
			continue
		}
		nr, ok := s.rs.LookupCompleted(ItemStepPath(mapStatic, mr.N, suffix))
		if !ok || nr.Outputs == nil {
			continue // compact: only items where the producer committed typed output
		}
		if len(ref.Segments) == 2 {
			out = append(out, nr.Outputs)
			continue
		}
		val, err := descendPath(nr.Outputs, ref.Segments[2:], "step."+idSeg.Ident+".")
		if err != nil {
			return nil, true, err
		}
		out = append(out, val)
	}
	return out, true, nil
}
