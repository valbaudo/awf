package ir

import (
	"reflect"
	"strings"
)

// validateUnknownKeys (AWF1062) rejects unknown top-level and step/control keys.
//
// The tolerant unmarshal layer (node_unmarshal.go) silently drops any key it does
// not recognize, so a typo'd `ouput_files:` quietly discards the artifact contract
// and GHA muscle-memory keys (`env:` on a step, `working-directory:`) do nothing.
// This pass walks the RAW pre-typed tree (LoadedModule.RawDoc) and flags every key
// that is not a json-tag field of the shape it belongs to.
//
// Allowed-key sets come from json-tag reflection over the IR structs
// (ir/tags_test.go enforces every exported field has a non-empty tag), so they
// track the structs automatically. Node classification reuses the
// controlKeys/stepKeys registries — the same single-discriminator rule as
// unmarshalNode. Child recursion (into then/else/body/generate/…) is an explicit
// per-kind switch that mirrors WalkNodes/unmarshalControl, because the raw tree's
// node-bearing children live under branch keys, not reflectable struct fields.
//
// Deliberately NOT walked: value subtrees that are opaque or free-form —
// `with:` (adapter config the core never reads), JSON-Schema values
// (output_schema, workflow input, tool input_schema), and workflow `outputs:`
// values. The walker only descends into the graph and control-node child node
// lists, so those subtrees are skipped by construction (they are not node lists).
//
// RawDoc is nil for a module whose top level was not a mapping and for the
// unreachable Root() fallback — nil means "no strict check available", so the pass
// no-ops.
func validateUnknownKeys(mod validationModule, c *collector) {
	raw := mod.RawDoc
	if raw == nil {
		return
	}
	wfAllowed := allowedJSONKeys(reflect.TypeOf(Workflow{}))
	// Top-level x-* keys are reserved for YAML-anchor holders (spec-level convention),
	// so they are tolerated here even though the typed Workflow has no such field.
	checkUnknownKeys(c, "", raw, wfAllowed, true, "top-level key")

	graph, ok := raw["graph"].([]any)
	if !ok {
		return
	}
	walkRawNodeList("", graph, c)
}

// allowedJSONKeys returns the set of json-tag names on t's exported fields.
// (ir/tags_test.go enforces every IR struct field has a non-empty json tag.)
func allowedJSONKeys(t reflect.Type) map[string]struct{} {
	out := map[string]struct{}{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			out[name] = struct{}{}
		}
	}
	return out
}

// checkUnknownKeys emits AWF1062 for every key in m not present in allowed. label
// names the kind of key for the message ("top-level key", "step key", "key",
// "reduce key"). When tolerateX is set, x-* keys are skipped (YAML-anchor holders).
func checkUnknownKeys(c *collector, path string, m map[string]any, allowed map[string]struct{}, tolerateX bool, label string) {
	for k := range m {
		if tolerateX && strings.HasPrefix(k, "x-") {
			continue
		}
		if _, ok := allowed[k]; ok {
			continue
		}
		c.errf(path, "AWF1062", "unknown "+label+" "+quoteKey(k)+didYouMean(k, allowed))
	}
}

// walkRawNodeList walks a raw graph / child node list ([]any of map[string]any).
func walkRawNodeList(parent string, list []any, c *collector) {
	for i, elem := range list {
		m, ok := elem.(map[string]any)
		if !ok {
			continue // malformed element; the structural pass reports node-shape errors
		}
		walkRawNode(parent, i, m, c)
	}
}

// walkRawNode classifies one raw node map by its single discriminator key and
// diffs its keys against the reflected allowed set, then recurses into control
// children.
func walkRawNode(parent string, index int, m map[string]any, c *collector) {
	kind := rawNodeKind(m)
	if kind == "" {
		return // zero or multiple kind keys — parse/structural layer already reports this
	}
	if factory, isStep := stepKeys[kind]; isStep {
		allowed := allowedJSONKeys(derefType(reflect.TypeOf(factory())))
		path := PathFor(parent, "", rawStepID(m), index)
		// Step nodes are flat: the discriminator (run/uses/…) and every sibling
		// field are keys of m itself. None of the fields are node lists, so there
		// is nothing to recurse into (with:/output_schema/input_files are skipped
		// by not descending into their values).
		checkUnknownKeys(c, path, m, allowed, false, "step key")
		return
	}
	factory := controlKeys[kind]
	path := PathFor(parent, kind, "", index)
	// A control node is a single-key wrapper ({if: {...}}). Only the keyword is
	// allowed at the wrapper level; a stray sibling is an author mistake.
	checkUnknownKeys(c, path, m, map[string]struct{}{kind: {}}, false, "key")
	switch kind {
	case "parallel":
		// Wire form is {parallel: [<node>, ...]} — the value is the child list
		// itself, addressed under the bare parallel[i] path (matches WalkNodes).
		if arr, ok := m[kind].([]any); ok {
			walkRawNodeList(path, arr, c)
		}
		return
	case "skip":
		return // {skip: "<reason>"} — the value is a string, no keys to check
	}
	inner, ok := m[kind].(map[string]any)
	if !ok {
		return // malformed inner; the structural pass reports it
	}
	checkUnknownKeys(c, path, inner, allowedJSONKeys(derefType(reflect.TypeOf(factory()))), false, "key")
	recurseControlChildren(kind, path, inner, c)
}

// recurseControlChildren descends into a control node's child node lists,
// mirroring WalkNodes / unmarshalControl. The branch keys are the node-bearing
// fields; other inner keys are leaves already checked by walkRawNode.
func recurseControlChildren(kind, path string, inner map[string]any, c *collector) {
	child := func(branch string) {
		if arr, ok := inner[branch].([]any); ok {
			walkRawNodeList(path+"."+branch, arr, c)
		}
	}
	switch kind {
	case "if":
		child("then")
		child("else")
	case "loop":
		child("body")
	case "try":
		child("do")
		child("catch")
		child("finally")
	case "gate":
		child("generate")
		child("evaluate")
	case "map":
		child("body")
		// reduce is a nested object (no child node list); check its own keys.
		if red, ok := inner["reduce"].(map[string]any); ok {
			checkUnknownKeys(c, path+".reduce", red, allowedJSONKeys(reflect.TypeOf(Reduce{})), false, "reduce key")
		}
	case "compose":
		child("body")
	case "react":
		// No child node list — tools resolve to top-level tools:, not child nodes.
	}
}

// rawNodeKind returns the single control/step discriminator key present in m, or
// "" if none or more than one is present (the same rule unmarshalNode enforces).
func rawNodeKind(m map[string]any) string {
	found := ""
	count := 0
	for k := range m {
		if _, ok := controlKeys[k]; ok {
			found, count = k, count+1
		} else if _, ok := stepKeys[k]; ok {
			found, count = k, count+1
		}
	}
	if count == 1 {
		return found
	}
	return ""
}

func rawStepID(m map[string]any) string {
	if id, ok := m["id"].(string); ok {
		return id
	}
	return ""
}

func derefType(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return t
}

func quoteKey(k string) string { return "\"" + k + "\"" }

// didYouMean returns a `(did you mean "x"?)` suffix for the closest allowed key
// within edit distance 2, or "" if none is close. Ties break on the lexically
// smallest candidate so the message is deterministic despite map iteration order.
func didYouMean(key string, allowed map[string]struct{}) string {
	const maxDist = 2
	best := ""
	bestDist := maxDist + 1
	for cand := range allowed {
		d := levenshtein(key, cand)
		if d < bestDist || (d == bestDist && cand < best) {
			bestDist, best = d, cand
		}
	}
	if best == "" || bestDist > maxDist {
		return ""
	}
	return " (did you mean " + quoteKey(best) + "?)"
}

// levenshtein is the standard edit distance (single-row DP). Keys are short, so
// the O(len(a)*len(b)) cost is negligible.
func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr := make([]int, len(b)+1)
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev = curr
	}
	return prev[len(b)]
}
