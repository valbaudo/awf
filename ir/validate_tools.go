package ir

// reservedReactOutputFields are engine-injected siblings of a react: node's typed
// output; an output_schema may not declare them. See spec §5/§6.1.
// (Mirrors the QuorumVerdictFields pattern in validate_reduce.go.)
var reservedReactOutputFields = []string{"stop_reason"}

// validateTools checks the top-level tools: map and all react: nodes in ld:
//
//   - AWF1056: every tool impl must name a containers:-declared container.
//   - AWF1052: react: tools list must be non-empty.
//   - AWF1053: every name in react: tools must appear in the top-level tools: map.
//   - AWF1054: react: max_turns must be non-negative (0 = default 8; negative rejected).
//   - AWF1055: react: output_schema may not declare a property named stop_reason.
//   - AWF1057: react: uses must be "awf/llm" (the only Containerless+Threaded adapter).
//   - AWF1058: react: structured_output: ollama_format is incompatible with tool calls.
//
// Note on AWF1057: if react.with.uses names an agents: role (resolved later), the
// static literal check cannot see through it. The run-start defensive gate
// (Phase 4, Caps.Containerless && Caps.Threaded assertion) is the authoritative gate
// for the role-alias case; AWF1057 catches the common literal mistake early.
func validateTools(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	wf := ld.Workflow

	// --- Tool definitions (AWF1056) ---
	for name, tool := range wf.Tools {
		path := "tools." + name
		if tool.Impl.Container == "" {
			c.errf(path, "AWF1056", catalog["AWF1056"])
			continue
		}
		if _, ok := wf.Containers[tool.Impl.Container]; !ok {
			c.errf(path, "AWF1056", catalog["AWF1056"])
		}
	}

	// --- react: nodes (AWF1052–AWF1055, AWF1057–AWF1058) ---
	// Uses the 3-arg WalkNodes(list, parent, visit(node, path)) — see ir/walk.go.
	WalkNodes(wf.Graph, "", func(n Node, path string) {
		r, ok := n.(*React)
		if !ok {
			return
		}

		// AWF1052: tools list must be non-empty.
		if len(r.Tools) == 0 {
			c.errf(path, "AWF1052", catalog["AWF1052"])
		}

		// AWF1053: every named tool must be declared in the top-level tools: map.
		for _, tn := range r.Tools {
			if _, ok := wf.Tools[tn]; !ok {
				c.errf(path, "AWF1053", catalog["AWF1053"])
			}
		}

		// AWF1054: max_turns must be non-negative (0 means "default 8").
		if r.MaxTurns < 0 {
			c.errf(path, "AWF1054", catalog["AWF1054"])
		}

		// AWF1055: output_schema may not declare stop_reason (engine-reserved).
		if r.OutputSchema != nil {
			if props, ok := (*r.OutputSchema)["properties"].(map[string]any); ok {
				for _, reserved := range reservedReactOutputFields {
					if _, clash := props[reserved]; clash {
						c.errf(path, "AWF1055", catalog["AWF1055"])
					}
				}
			}
		}

		// AWF1057: adapter must be awf/llm (the only Containerless+Threaded adapter in v1).
		uses, _ := r.With["uses"].(string)
		if uses != "awf/llm" {
			c.errf(path, "AWF1057", catalog["AWF1057"])
		}

		// AWF1058: ollama_format structured output is incompatible with tool calls.
		if so, _ := r.With["structured_output"].(string); so == "ollama_format" {
			c.errf(path, "AWF1058", catalog["AWF1058"])
		}
	})
}
