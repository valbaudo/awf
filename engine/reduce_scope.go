package engine

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/gowebpki/jcs"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

type reduceTemplateScope struct {
	base    *Scope
	mapPath string
}

// reduceCallContext carries the call sub-workflow frame (runtimeParent prefix +
// typed call input/files) into the reduce executor so a reducer's run:/output_files
// templates resolve against the SAME scope the rest of the called sub-workflow
// uses. The zero value (empty runtimeParent, nil maps) is a top-level workflow —
// the reduce scope then reduces to the historical NewScope(rs, wf, mapPath).
type reduceCallContext struct {
	runtimeParent string
	input         map[string]any
	inputFiles    map[string]string
}

// newReduceTemplateScope builds a reducer template scope for callers that already
// hold mapPath and rs in the SAME prefix-frame (the static-map-path artifact
// resolvers in artifact_scope.go / artifact_refs.go). For the reduce EXECUTOR —
// where mapPath is a runtime path against the parent rs and the reducer lives in a
// possibly-called sub-workflow — use newReduceTemplateScopeForExec, which honors
// the call frame.
func newReduceTemplateScope(rs *RunState, wf *ir.Workflow, mapPath string) *reduceTemplateScope {
	return newReduceTemplateScopeForExec(rs, wf, mapPath, reduceCallContext{})
}

// newReduceTemplateScopeForExec builds the reducer's template scope honoring an
// optional call frame (cc). The base scope and the map path are BOTH taken in the
// child (prefix-stripped) frame when cc.runtimeParent is set, so outer-sibling /
// input refs AND body-step aggregate refs (which compare against ir.SingleMapBodyShape's
// static map path) resolve correctly inside a called sub-workflow. With a zero cc
// this is identical to the historical NewScope(rs, wf, mapPath) behavior.
func newReduceTemplateScopeForExec(rs *RunState, wf *ir.Workflow, mapPath string, cc reduceCallContext) *reduceTemplateScope {
	base := callContextScope(rs, wf, mapPath, cc.runtimeParent, cc.input, cc.inputFiles)
	return &reduceTemplateScope{base: base, mapPath: stripRuntimeParent(mapPath, cc.runtimeParent)}
}

func (s *reduceTemplateScope) Resolve(ref *template.Ref) (any, error) {
	staticPath, ok := s.bodyStepPath(ref)
	if !ok {
		return s.base.Resolve(ref)
	}
	v, err := s.resolveBodyStepAggregate(staticPath, ref)
	if err != nil {
		return nil, err
	}
	return renderReduceTemplateJSON(v)
}

func (s *reduceTemplateScope) bodyStepPath(ref *template.Ref) (string, bool) {
	if ref == nil || len(ref.Segments) < 2 || ref.Segments[0].Ident != "step" || ref.Segments[1].IsIndex {
		return "", false
	}
	staticPath, ok := s.base.stepIndex[ref.Segments[1].Ident]
	if !ok {
		return "", false
	}
	mapPath, _, ok := ir.SingleMapBodyShape(staticPath)
	return staticPath, ok && mapPath == s.mapPath
}

func (s *reduceTemplateScope) resolveBodyStepAggregate(staticPath string, ref *template.Ref) ([]any, error) {
	mapStatic, suffix, ok := ir.SingleMapBodyShape(staticPath)
	if !ok || mapStatic != s.mapPath {
		return nil, template.EvalErrf(template.EvalCodeRefUnresolved, "step aggregate is not inside reducer map %q", s.mapPath)
	}
	items := s.base.rs.LookupMapItems(mapStatic)
	sort.Slice(items, func(i, j int) bool { return items[i].N < items[j].N })
	out := []any{}
	for _, mr := range items {
		if mr.Status != ItemPassed {
			continue
		}
		nr, ok := s.base.rs.LookupCompleted(ItemStepPath(mapStatic, mr.N, suffix))
		if !ok || nr.Outputs == nil {
			continue
		}
		if len(ref.Segments) == 2 {
			out = append(out, nr.Outputs)
			continue
		}
		val, err := descendPath(nr.Outputs, ref.Segments[2:], "step."+ref.Segments[1].Ident+".")
		if err != nil {
			return nil, err
		}
		out = append(out, val)
	}
	return out, nil
}

func renderReduceTemplateJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal reducer aggregate ref: %w", err)
	}
	canon, err := jcs.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize reducer aggregate ref: %w", err)
	}
	return string(canon), nil
}
