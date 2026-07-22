package engine

import (
	"fmt"
	"strings"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// ResolveDeclaredArtifactPath resolves a producer id + its declared output_files
// path to the committed CAS blob ref. Most step artifacts substitute the declared
// path in the consumer scope to mirror capture-time substitution. Named reduced
// map artifacts are different: the reducer owned capture-time substitution, so
// the declared path is rendered against the map product's reducer scope.
func (s *Scope) ResolveDeclaredArtifactPath(id, declaredPath string) (string, error) {
	if mp, ok := s.mapProducts[id]; ok && mp.reduce {
		if s.wfRef == nil {
			return "", template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: map product %q cannot resolve declared path without workflow", id)
		}
		containerPath, err := template.Substitute(declaredPath, newReduceTemplateScope(s.rs, s.wfRef, mp.mapPath))
		if err != nil {
			return "", fmt.Errorf("substitute map product artifact path %q: %w", declaredPath, err)
		}
		return s.resolveMapProductArtifactPath(id, mp, containerPath)
	}
	containerPath, err := template.Substitute(declaredPath, s)
	if err != nil {
		return "", fmt.Errorf("substitute artifact path %q: %w", declaredPath, err)
	}
	return s.ResolveArtifactPath(id, containerPath)
}

// ResolveArtifactPath resolves a producer step id + its substituted container
// path to the committed CAS blob ref (NodeResult.Files[path], which is PATH-keyed).
// Reuses stepIndex + stepRuntimePath so map/loop/gate multiplicity is handled
// identically to scalar refs.
func (s *Scope) ResolveArtifactPath(id, containerPath string) (string, error) {
	staticPath, ok := s.stepIndex[id]
	if !ok {
		if mp, ok := s.mapProducts[id]; ok && mp.reduce {
			return s.resolveMapProductArtifactPath(id, mp, containerPath)
		}
		return "", template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: step %q not declared", id)
	}
	// C2a (reduce): symmetric to aggregateMapOutputs — if the producer sits in a
	// single-map body, the map declared a reduce:, and the reducer committed a
	// NodeResult at the map's own path, the artifact resolves to the REDUCER's
	// file, not the per-item body artifact. A non-reduce map never commits at the
	// bare map path, so this misses and per-item resolution below runs unchanged.
	if mapStatic, _, isMapBody := ir.SingleMapBodyShape(staticPath); isMapBody {
		if _, inside, ierr := s.instanceFromCtx(mapStatic, itemSep); ierr == nil && !inside {
			if nr, ok := s.rs.LookupCompleted(mapStatic); ok {
				cas, ok := nr.Files[containerPath]
				if !ok {
					return "", template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: reduced map %q has no committed artifact at %q", id, containerPath)
				}
				return cas, nil
			}
		}
	}
	if rp, handled, err := s.passedGateArtifactRuntimePath(staticPath); handled {
		if err != nil {
			return "", err
		}
		nr, ok := s.rs.LookupCompleted(rp)
		if !ok {
			return "", template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: passed gate step %q not yet committed (%s)", id, rp)
		}
		cas, ok := nr.Files[containerPath]
		if !ok {
			return "", template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: passed gate step %q has no committed artifact at %q", id, containerPath)
		}
		return cas, nil
	}
	rp, err := s.stepRuntimePath(staticPath)
	if err != nil {
		return "", &template.EvalError{Code: template.EvalCodeRefUnresolved, Msg: err.Error()}
	}
	nr, ok := s.rs.LookupCompleted(rp)
	if !ok {
		if s.absentDueToUntakenIf(rp) {
			return "", template.EvalErrf(template.EvalCodeRefAbsent, "artifact ref: step %q is under a non-taken if branch (%s)", id, rp)
		}
		return "", template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: step %q not yet committed (%s)", id, rp)
	}
	cas, ok := nr.Files[containerPath]
	if !ok {
		return "", template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: step %q has no committed artifact at %q", id, containerPath)
	}
	return cas, nil
}

func (s *Scope) resolveMapProductArtifactPath(id string, mp mapProduct, containerPath string) (string, error) {
	nr, ok := s.rs.LookupCompleted(mp.mapPath)
	if !ok {
		return "", template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: map product %q not yet committed (%s)", id, mp.mapPath)
	}
	cas, ok := nr.Files[containerPath]
	if !ok {
		return "", template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: map product %q has no committed artifact at %q", id, containerPath)
	}
	return cas, nil
}

// attemptPath splices the ACCEPTED gate attempt into fullPath: it scans attempts
// newest-first for the last AttemptPassed and replaces gatePrefix with
// AttemptPath(gatePrefix, n). Returns "" if no passed attempt exists (the gate
// did not run, or was rejected). Shared by passedGateArtifactRuntimePath (the
// sequential input_files exception) and itemBodyStepPath (reduce fan-in) so the
// forwarding rule lives in one place.
func attemptPath(fullPath, gatePrefix string, attempts []AttemptResult) string {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].AttemptOutcome == AttemptPassed {
			return strings.Replace(fullPath, gatePrefix, AttemptPath(gatePrefix, attempts[i].N), 1)
		}
	}
	return ""
}

func (s *Scope) passedGateArtifactRuntimePath(staticPath string) (string, bool, error) {
	gateStatic, ok := gateScopePrefix(staticPath)
	if !ok || runtimePathWithinGate(s.ctxPath, gateStatic) {
		return "", false, nil
	}
	// A passed gate is transparent to its generate: subtree ONLY — the
	// evaluator's artifacts stay gate-internal, same as its scalar verdict.
	// Mirrors stepRuntimePath's gate arm (engine/scope.go); validation
	// enforces this too (ir.blockingScope).
	if !strings.HasPrefix(strings.TrimPrefix(staticPath, gateStatic+"."), "generate.") {
		return "", true, template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: step inside gate %q is not referenceable from outside: only the gate's generate: producers forward; the evaluator's artifacts stay gate-internal", gateStatic)
	}
	if p := attemptPath(staticPath, gateStatic, s.rs.LookupGateAttempts(gateStatic)); p != "" {
		return p, true, nil
	}
	return "", true, template.EvalErrf(template.EvalCodeRefUnresolved, "artifact ref: step inside gate %q has no passed attempt", gateStatic)
}

// itemBodyStepPath returns the committed runtime path of a map body producer for
// item n, and whether it resolved. A plain producer resolves to
// ItemStepPath(mapPath, n, suffix). A producer nested in a single gate ran across
// attempts and committed at an attempt-suffixed path; this splices in the
// ACCEPTED attempt via the shared attemptPath helper. ok is false when the
// producer is gate-nested but the gate did not run / has no passed attempt for
// this item (e.g. a gate in a not-taken if-branch) → the caller compacts.
// Producers under a loop or >1 gate are rejected at validation (AWF5007), so this
// handles at most one gate.
func itemBodyStepPath(rs *RunState, mapPath string, n int, suffix string) (string, bool) {
	gateRel, isGate := gateScopePrefix(suffix)
	if !isGate {
		return ItemStepPath(mapPath, n, suffix), true
	}
	itemGatePath := ItemStepPath(mapPath, n, gateRel)
	if p := attemptPath(ItemStepPath(mapPath, n, suffix), itemGatePath, rs.LookupGateAttempts(itemGatePath)); p != "" {
		return p, true
	}
	return "", false
}

func gateScopePrefix(staticPath string) (string, bool) {
	segs := strings.Split(staticPath, ".")
	for i := len(segs) - 1; i >= 0; i-- {
		if strings.HasPrefix(segs[i], "gate[") {
			return strings.Join(segs[:i+1], "."), true
		}
	}
	return "", false
}

func runtimePathWithinGate(ctxPath, gateStatic string) bool {
	return ctxPath == gateStatic || strings.HasPrefix(ctxPath, gateStatic+attemptSep)
}
