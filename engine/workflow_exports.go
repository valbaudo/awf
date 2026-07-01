package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

type WorkflowExportResult struct {
	Outputs map[string]any
	Files   map[string]string
}

// EvaluateExports evaluates a workflow's outputs:/output_schema/output_files
// against an ALREADY-CORRECT (rs, ctxPath, input). The caller constructs rs:
// a sub-workflow call prefix-strips via childRunStateForCall and passes the
// parent path; a top-level run (awf outputs) passes the folded RunState
// directly with ctxPath="" and input=nil. Shared so both paths are one
// implementation (mirrors the engine's top-level-vs-call split in
// interpreter_context.go).
//
// This def-unaware entry does NOT classify cross-call ABSENT (a parent output
// bound to a child-omitted optional output resolves to AWF4002). Callers that
// hold the LoadedDefinition should use EvaluateExportsInDef so that a
// {{ step.<call>.<field> }} ref to a child-declared-but-omitted output resolves
// to AWF4006 (parent omits) instead — Part C C6.
func EvaluateExports(rs *RunState, wf *ir.Workflow, ctxPath string, input map[string]any, blobs state.Blobs) (WorkflowExportResult, error) {
	return EvaluateExportsInDef(nil, "", rs, wf, ctxPath, input, blobs)
}

// EvaluateExportsInDef is EvaluateExports with the LoadedDefinition + evaluating
// module id threaded in, so a parent {{ step.<call>.<field> }} ref can reach the
// child workflow's output_schema. When the child DECLARED that field optional but
// OMITTED it (its producer sat under a non-taken if branch, per C3), the ref
// resolves to AWF4006 and the parent OMITS the key — composing the single-workflow
// omit across a sub-workflow call. A field the child never declared stays AWF4002
// (a genuine typo). def==nil disables the classification (identical to the historic
// EvaluateExports behavior).
func EvaluateExportsInDef(def *ir.LoadedDefinition, moduleID string, rs *RunState, wf *ir.Workflow, ctxPath string, input map[string]any, blobs state.Blobs) (WorkflowExportResult, error) {
	scope := NewScopeWithInput(rs, wf, ctxPath, input)
	scope.callChildSchemas = callChildOutputSchemas(def, moduleID, wf)

	var out WorkflowExportResult
	if wf.OutputSchema != nil {
		out.Outputs = map[string]any{}
	}
	if len(wf.Outputs) > 0 {
		out.Outputs = make(map[string]any, len(wf.Outputs))
		keys := make([]string, 0, len(wf.Outputs))
		for key := range wf.Outputs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if refs, ok := firstOfRefs(wf.Outputs[key]); ok {
				value, absent, err := evalFirstOf(refs, scope)
				if err != nil {
					return WorkflowExportResult{}, fmt.Errorf("evaluate workflow output %q: %w", key, err)
				}
				if !absent {
					out.Outputs[key] = value
				}
				continue
			}
			value, err := template.EvalTemplateValue(wf.Outputs[key], scope)
			if err != nil {
				// AWF4006 (EvalCodeRefAbsent): the binding names a step under a
				// non-taken `if` branch — legitimately ABSENT. OMIT the key (don't
				// set it); ValidateOutputMap below then enforces output_schema
				// required-ness (omitted+required → hard fail; omitted+optional →
				// pass). errors.As (not a bare assertion) handles the wrapped case:
				// a mixed string like "x-{{ step.deep.summary }}" routes through
				// Substitute, which re-wraps the EvalError while preserving Code.
				var ee *template.EvalError
				if errors.As(err, &ee) && ee.Code == template.EvalCodeRefAbsent {
					continue
				}
				return WorkflowExportResult{}, fmt.Errorf("evaluate workflow output %q: %w", key, err)
			}
			out.Outputs[key] = value
		}
	}
	if wf.OutputSchema != nil {
		if err := ValidateOutputMap(out.Outputs, wf.OutputSchema); err != nil {
			return WorkflowExportResult{}, fmt.Errorf("workflow output_schema validation: %w", err)
		}
	}

	if len(wf.ArtifactExports) > 0 {
		out.Files = make(map[string]string, len(wf.ArtifactExports))
		keys := make([]string, 0, len(wf.ArtifactExports))
		for key := range wf.ArtifactExports {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			raw := wf.ArtifactExports[key]
			if strings.Contains(raw, "{{") || strings.Contains(raw, "}}") {
				return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s must be a static step.<id>.files.<name> reference, not a template", key)
			}
			id, name, ok := template.ParseArtifactRef(raw)
			if !ok {
				return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s=%s: expected step.<id>.files.<name>", key, raw)
			}
			ref, err := resolveNamedArtifactRef(scope, wf, id, name)
			if err != nil {
				var ee *template.EvalError
				if errors.As(err, &ee) && ee.Code == template.EvalCodeRefAbsent {
					continue // omit, symmetric with outputs:
				}
				return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s: %w", key, err)
			}
			if blobs != nil {
				if _, err := blobs.Get(ref); err != nil {
					return WorkflowExportResult{}, fmt.Errorf("workflow output_files.%s ref %q is missing from blobs: %w", key, ref, err)
				}
			}
			out.Files[key] = ref
		}
	}

	return out, nil
}

// firstOfRefs recognizes the reserved `first_of` directive: a single-key object
// {"first_of": [ <templateValue>, ... ]} at an outputs-value ROOT. Returns the
// element raw messages and ok=true only for that exact shape (so an author object
// merely containing a "first_of" key among others is NOT a directive).
func firstOfRefs(raw ir.TemplateValue) ([]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || len(obj) != 1 {
		return nil, false
	}
	inner, ok := obj["first_of"]
	if !ok {
		return nil, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(inner, &arr); err != nil || arr == nil {
		// arr == nil rejects `{"first_of": null}`: `null` unmarshals into a slice
		// without error but is not a JSON array, so it is not the directive — fall
		// through to the generic path rather than silently treating it as empty.
		return nil, false
	}
	return arr, true
}

// evalFirstOf resolves refs left-to-right: first non-ABSENT wins; all ABSENT →
// absent=true (caller omits the key). A non-ABSENT EvalError (or any other error)
// fails the export.
func evalFirstOf(refs []json.RawMessage, scope *Scope) (any, bool, error) {
	for _, r := range refs {
		v, err := template.EvalTemplateValue(r, scope)
		if err != nil {
			var ee *template.EvalError
			if errors.As(err, &ee) && ee.Code == template.EvalCodeRefAbsent {
				continue
			}
			return nil, false, err
		}
		return v, false, nil
	}
	return nil, true, nil
}

// evaluateWorkflowExports is the sub-workflow-CALL path: build the child
// RunState (prefix-strip the parent's keys) then delegate to the shared
// EvaluateExportsInDef. Call-specific construction stays here. def + moduleID are
// the CHILD's (so a child forwarding ITS OWN call's omitted optional output
// composes too — C6).
func evaluateWorkflowExports(def *ir.LoadedDefinition, moduleID string, parent *RunState, wf *ir.Workflow, callPath string, input map[string]any, blobs state.Blobs) (WorkflowExportResult, error) {
	child := childRunStateForCall(parent, callPath, input)
	return EvaluateExportsInDef(def, moduleID, child, wf, ir.CallWorkflowParentPath(callPath), input, blobs)
}

func childRunStateForCall(parent *RunState, callPath string, input map[string]any) *RunState {
	child := NewRunState(parent.RunID, parent.WorkflowDigest, input)
	prefix := ir.CallWorkflowParentPath(callPath)
	parent.mu.Lock()
	defer parent.mu.Unlock()
	copyPrefixedCompleted(child.Completed, parent.Completed, prefix)
	copyPrefixedBranches(child.Branches, parent.Branches, prefix)
	copyPrefixedLoopIters(child.LoopIters, parent.LoopIters, prefix)
	copyPrefixedGateAttempts(child.GateAttempts, parent.GateAttempts, prefix)
	copyPrefixedReactRounds(child.ReactRounds, parent.ReactRounds, prefix)
	copyPrefixedMapItems(child.MapItems, parent.MapItems, prefix)
	return child
}

func copyPrefixedCompleted(dst, src map[string]NodeResult, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			dst[childPath] = value
		}
	}
}

func copyPrefixedBranches(dst, src map[string]string, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			dst[childPath] = value
		}
	}
}

func copyPrefixedLoopIters(dst, src map[string]int, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			dst[childPath] = value
		}
	}
}

func copyPrefixedGateAttempts(dst, src map[string][]AttemptResult, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			cp := make([]AttemptResult, len(value))
			copy(cp, value)
			dst[childPath] = cp
		}
	}
}

func copyPrefixedMapItems(dst, src map[string][]MapItemRecord, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			cp := make([]MapItemRecord, len(value))
			copy(cp, value)
			dst[childPath] = cp
		}
	}
}

func copyPrefixedReactRounds(dst, src map[string][]ReactRoundRecord, prefix string) {
	for path, value := range src {
		if childPath, ok := stripChildPrefix(path, prefix); ok {
			cp := make([]ReactRoundRecord, len(value))
			copy(cp, value)
			dst[childPath] = cp
		}
	}
}

func stripChildPrefix(path, prefix string) (string, bool) {
	if path == prefix {
		return "", true
	}
	prefix += "."
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}
