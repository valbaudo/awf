package ir

// validateDurationScalars (AWF1063) rejects a bare-integer YAML duration value at
// every position the IR types as a *Duration: a step's `timeout`, its
// `retry.initial`/`retry.max`, and a top-level tool's `timeout`.
//
// Duration.UnmarshalJSON (ir/types.go) stays permissive on purpose — it must keep
// parsing a bare int as nanoseconds so the `awf ui` definition-snapshot round-trip
// (engine/definition_snapshot.go MarshalJSON -> ui/server.go UnmarshalJSON) is
// untouched. `timeout: 300` therefore silently parses as 300 NANOSECONDS with no
// runtime error — the step times out instantly. This pass catches that statically
// over the raw pre-typed tree (LoadedModule.RawDoc) instead of tightening the
// unmarshaler. A value is a violation only when the key is present and its RawDoc
// value is not a string; goccy decodes `300` to a JSON number and `"300s"` to a
// string.
//
// The graph walk is a deliberate parallel structure to validateUnknownKeys'
// walkRawNodeList / walkRawNode / recurseControlChildren (validate_unknown_keys.go):
// same node classification (rawNodeKind, stepKeys, controlKeys), same control-child
// branches per kind, same implicit with:-subtree skip (neither pass ever ranges
// over a node map's keys — each inspects only the specific keys it cares about, so
// with: is never descended into). Kept as a SEPARATE pass (not a shared visitor)
// so AWF1062 behavior stays byte-for-byte unchanged; see task-9 report for the
// alternative considered.
func validateDurationScalars(mod validationModule, c *collector) {
	raw := mod.RawDoc
	if raw == nil {
		return
	}
	if graph, ok := raw["graph"].([]any); ok {
		walkDurationNodeList("", graph, c)
	}
	// tools: is a top-level map (name -> definition), not a graph node list. The
	// Duration fields (ToolImpl.Timeout / .Retry, ir/tool.go) live under the
	// entry's impl: sub-map, not on the entry itself.
	if tools, ok := raw["tools"].(map[string]any); ok {
		for name, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			impl, ok := tm["impl"].(map[string]any)
			if !ok {
				continue
			}
			path := "tools." + name + ".impl"
			checkDurationKey(impl, "timeout", path, c)
			if retry, ok := impl["retry"].(map[string]any); ok {
				checkDurationKey(retry, "initial", path+".retry", c)
				checkDurationKey(retry, "max", path+".retry", c)
			}
		}
	}
}

// walkDurationNodeList walks a raw graph / child node list ([]any of
// map[string]any), mirroring walkRawNodeList.
func walkDurationNodeList(parent string, list []any, c *collector) {
	for i, elem := range list {
		m, ok := elem.(map[string]any)
		if !ok {
			continue // malformed element; the structural pass reports node-shape errors
		}
		walkDurationNode(parent, i, m, c)
	}
}

// walkDurationNode classifies one raw node map by its single discriminator key
// (rawNodeKind, shared with validateUnknownKeys). Step nodes are flat: `timeout`
// and `retry` are keys of m itself, so there is nothing to recurse into. Control
// nodes carry no Duration field of their own (ir/node.go) — recurse into their
// child node lists only.
func walkDurationNode(parent string, index int, m map[string]any, c *collector) {
	kind := rawNodeKind(m)
	if kind == "" {
		return // zero or multiple kind keys — parse/structural layer already reports this
	}
	if _, isStep := stepKeys[kind]; isStep {
		path := PathFor(parent, "", rawStepID(m), index)
		checkDurationKey(m, "timeout", path, c)
		if retry, ok := m["retry"].(map[string]any); ok {
			checkDurationKey(retry, "initial", path+".retry", c)
			checkDurationKey(retry, "max", path+".retry", c)
		}
		return
	}
	path := PathFor(parent, kind, "", index)
	switch kind {
	case "parallel":
		// Wire form is {parallel: [<node>, ...]} — the value is the child list itself.
		if arr, ok := m[kind].([]any); ok {
			walkDurationNodeList(path, arr, c)
		}
		return
	case "skip":
		return // {skip: "<reason>"} — the value is a string, nothing to check
	}
	inner, ok := m[kind].(map[string]any)
	if !ok {
		return // malformed inner; the structural pass reports it
	}
	recurseDurationControlChildren(kind, path, inner, c)
}

// recurseDurationControlChildren descends into a control node's child node lists,
// mirroring recurseControlChildren. No control node type (If/Loop/Try/Gate/Map/
// Compose/React) declares a Timeout or RetryPolicy field of its own — only step
// nodes do — so this only needs to recurse, never check keys on the wrapper.
func recurseDurationControlChildren(kind, path string, inner map[string]any, c *collector) {
	child := func(branch string) {
		if arr, ok := inner[branch].([]any); ok {
			walkDurationNodeList(path+"."+branch, arr, c)
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
		// reduce: (ir.Reduce) has no Timeout/Retry field — nothing to check there.
	case "compose":
		child("body")
	case "react":
		// No child node list, and React itself has no Timeout/Retry field.
	}
}

// checkDurationKey emits AWF1063 when m[key] is present and its RawDoc value is
// not a string.
func checkDurationKey(m map[string]any, key, path string, c *collector) {
	v, ok := m[key]
	if !ok {
		return
	}
	if _, isStr := v.(string); !isStr {
		c.errf(path, "AWF1063", key+": "+catalog["AWF1063"])
	}
}
