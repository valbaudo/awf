package graph

import (
	"encoding/json"
	"sort"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/template"
)

// producerRefs returns the step ids a node references in its templated fields — its data
// dependencies — deduped, in first-seen order. It reuses the template package's parser
// (Slots/ParseRef for `{{ }}` strings, ParseExpr/References for bare exprs,
// ParseArtifactRef for input_files) rather than scanning text itself, so it can never
// drift from the real templating grammar. A parse error yields no refs for that field
// (the projection is best-effort; validation is the validator's job, not the graph's).
func producerRefs(n ir.Node) []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	addSlots := func(s string) {
		for _, id := range slotRefs(s) {
			add(id)
		}
	}
	addExpr := func(s string) {
		for _, id := range exprRefs(s) {
			add(id)
		}
	}
	addFiles := func(m map[string]string) {
		keys := make([]string, 0, len(m))
		for key := range m {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if id, _, ok := template.ParseArtifactRef(m[key]); ok {
				add(id)
			}
		}
	}

	switch v := n.(type) {
	case *ir.CodeStep:
		addSlots(v.Run)
		addFiles(v.InputFiles)
		if v.IdempotencyKey != nil {
			addSlots(string(*v.IdempotencyKey))
		}
	case *ir.AgentStep:
		addRawConfigStrings(v.With, addSlots)
		addFiles(v.InputFiles)
		if v.IdempotencyKey != nil {
			addSlots(string(*v.IdempotencyKey))
		}
		add(v.Continues) // continues: <id> is a thread dependency on a prior agent step
	case *ir.SignalStep:
		addExpr(v.Where)
	case *ir.CallStep:
		addTemplateValues(v.Input, addSlots)
		addFiles(v.InputFiles)
	case *ir.If:
		addExpr(string(v.Cond))
	case *ir.Loop:
		if v.Until != nil {
			addExpr(string(*v.Until))
		}
	case *ir.Gate:
		addExpr(string(v.Until))
	case *ir.Map:
		addExpr(string(v.Over))
		addSlots(string(v.Image))
	case *ir.Compose:
		if id, _, ok := template.ParseArtifactRef(v.From); ok {
			add(id)
		}
		addSlots(string(v.Service))
	}
	return ids
}

func addTemplateValues(values map[string]ir.TemplateValue, f func(string)) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var decoded any
		if err := json.Unmarshal(values[key], &decoded); err != nil {
			continue
		}
		addTemplateValueStrings(decoded, f)
	}
}

func addTemplateValueStrings(v any, f func(string)) {
	switch t := v.(type) {
	case string:
		f(t)
	case []any:
		for _, x := range t {
			addTemplateValueStrings(x, f)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for key := range t {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			addTemplateValueStrings(t[key], f)
		}
	}
}

// exprRefs extracts step-producer ids from a bare expression (if.cond, loop.until,
// gate.until, map.over, signal.where).
func exprRefs(s string) []string {
	if s == "" {
		return nil
	}
	e, err := template.ParseExpr(s)
	if err != nil {
		return nil
	}
	var ids []string
	for _, r := range template.References(e) {
		if id, ok := stepRefID(r); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// slotRefs extracts step-producer ids from a `{{ }}`-templated string (run, with values,
// image, idempotency_key).
func slotRefs(s string) []string {
	if s == "" {
		return nil
	}
	slots, err := template.Slots(s)
	if err != nil {
		return nil
	}
	var ids []string
	for _, sl := range slots {
		r, err := template.ParseRef(sl.Inner)
		if err != nil {
			continue
		}
		if id, ok := stepRefID(*r); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// stepRefID returns the referenced step id for a `step.<id>.…` ref, else ("", false).
// Other roots (input, the map `as` binding, gate `evaluate`) are not step producers.
func stepRefID(r template.Ref) (string, bool) {
	if len(r.Segments) >= 2 && r.Segments[0].Ident == "step" && !r.Segments[1].IsIndex {
		return r.Segments[1].Ident, true
	}
	return "", false
}

// addRawConfigStrings walks an opaque agent `with:` config and feeds every string value
// (recursing into nested maps/slices) to f. The config stays opaque to the core; this
// only reads it to find {{ }} refs for edge drawing, never to interpret harness config.
func addRawConfigStrings(v any, f func(string)) {
	switch t := v.(type) {
	case string:
		f(t)
	case map[string]any:
		for _, x := range t {
			addRawConfigStrings(x, f)
		}
	case ir.RawConfig:
		for _, x := range t {
			addRawConfigStrings(x, f)
		}
	case []any:
		for _, x := range t {
			addRawConfigStrings(x, f)
		}
	}
}
