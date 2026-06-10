package ir

import "fmt"

const CallWorkflowSegment = "workflow"

// PathFor computes the static IR path of a node for use in a Diagnostic. parent is the path
// of the enclosing node (empty at the top level); keyword is the control-node keyword (e.g.
// "if", "loop", "gate") and stepID is the step's id — exactly one of {keyword, stepID} is
// non-empty (a step has an id; a control node has a keyword); index is the node's position in
// its parent's children list (used only for control nodes — steps are addressed by id).
//
// If both are set (a caller bug), stepID takes precedence; the keyword is ignored. The
// "?[index]" fallback below covers the neither-set case.
//
// The runtime addressing function (engine/path, Phase 2) extends this with iter/attempt
// suffixes for in-flight nodes (e.g. `loop[0].body.iter-3`, `gate[0].attempt-2.generate`).
// The static prefix the validator emits matches the runtime form — `loop[0].body` is the
// same string in both, just without the per-iteration suffix.
//
// Per AWF standard §8: "step nodes by id; control nodes positionally — try[0].catch,
// if[1].then, loop[0].body.iter-3, gate[0].attempt-2.generate, parallel[2], map[0].item-3."
func PathFor(parent, keyword, stepID string, index int) string {
	var self string
	switch {
	case stepID != "":
		self = stepID
	case keyword != "":
		self = fmt.Sprintf("%s[%d]", keyword, index)
	default:
		// Should never happen — caller bug. Surface as "?[index]" rather than panicking.
		self = fmt.Sprintf("?[%d]", index)
	}
	if parent == "" {
		return self
	}
	return parent + "." + self
}

// ContainerPath builds a dotted path for container metadata, e.g.:
//
//	ContainerPath("lab", "image") → "containers.lab.image"
//	ContainerPath("lab", "")      → "containers.lab"
//
// Used by the structural and compose passes for container-attached diagnostics.
func ContainerPath(name, field string) string {
	if field == "" {
		return "containers." + name
	}
	return "containers." + name + "." + field
}

// ChildPath returns the path of a control node's named child block — the convention is
//
//	<parent>.<keyword>[idx].<branch>
//
// e.g. `if[1].then`, `loop[0].body`, `gate[2].generate`, `try[0].catch`. Centralizes the
// branch-label / path-join convention so every validation pass (validateStructural,
// validateRefs, validateSchema, indexProducers) addresses the same nested node identically.
// Without this helper, a typo in any one walker's `path+".then"` literal would silently
// produce a divergent path for the same node in different passes.
func ChildPath(parent, keyword string, idx int, branch string) string {
	return PathFor(parent, keyword, "", idx) + "." + branch
}

func CallWorkflowParentPath(callPath string) string {
	if callPath == "" {
		return CallWorkflowSegment
	}
	return callPath + "." + CallWorkflowSegment
}
