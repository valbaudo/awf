package engine

import "strings"

// Extracted from engine/scope.go during the slice 3.3 post-merge cleanup
// (split was 420 lines, 5 concerns). enclosingGateForEvaluate is a ctxPath
// walker — it doesn't read RunState and isn't bound to a Scope — so it lives
// alongside the runtime-addressing primitives in engine/path.go (and consumes
// the exported AttemptSegmentPrefix + GateSegmentPrefix constants from there).

// enclosingGateForEvaluate walks ctxPath looking for the INNERMOST gate whose
// generate or until subtree contains the current node. Returns the gate's
// runtime path (e.g. "gate[0]" for a top-level gate; "gate[0].attempt-1.generate.gate[2]"
// for a nested gate inside the outer's generate).
//
// Patterns recognized as "inside generate / until":
//   - <prefix>.gate[N].attempt-K.generate.<rest>  — anywhere under a generate subtree
//   - <prefix>.gate[N].attempt-K.until            — the gate.until expression (terminal)
//
// gate.evaluate's subtree is NOT a valid context — evaluate.* there would
// reference the evaluator's own in-flight output (chicken-and-egg). The
// walker returns the gate path only if the innermost matching segment is
// "generate" or "until" — NOT "evaluate".
//
// Nested gates: the rightmost matching triple wins. If a nested gate's
// matching segment is "evaluate", the walker keeps walking left to find the
// next-outer gate's generate (the test
// TestEnclosingGateForEvaluateTable's "gate.evaluate inside outer.generate"
// case pins this).
func enclosingGateForEvaluate(ctxPath string) (string, bool) {
	if ctxPath == "" {
		return "", false
	}
	segments := strings.Split(ctxPath, ".")
	// Walk from end backward. For each segment i ∈ ["generate", "until"]:
	//   * segments[i-1] must start with AttemptSegmentPrefix
	//   * segments[i-2] must start with GateSegmentPrefix
	//   * Return the join of segments[:i-1] (i.e., up to and including gate[N]).
	for i := len(segments) - 1; i >= 2; i-- {
		seg := segments[i]
		if seg != "generate" && seg != "until" {
			continue
		}
		if !strings.HasPrefix(segments[i-1], AttemptSegmentPrefix) {
			continue
		}
		if !strings.HasPrefix(segments[i-2], GateSegmentPrefix) {
			continue
		}
		// Gate path = segments[0:i-1] joined by ".".
		return strings.Join(segments[:i-1], "."), true
	}
	return "", false
}

func isGateEvaluateContext(ctxPath string) bool {
	if ctxPath == "" {
		return false
	}
	segments := strings.Split(ctxPath, ".")
	for i := len(segments) - 1; i >= 2; i-- {
		if segments[i] != "evaluate" {
			continue
		}
		if !strings.HasPrefix(segments[i-1], AttemptSegmentPrefix) {
			continue
		}
		if !strings.HasPrefix(segments[i-2], GateSegmentPrefix) {
			continue
		}
		return true
	}
	return false
}
