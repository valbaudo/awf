package ir

// ValidateJury checks every AgentStep carrying a jury: block. Runs in the loader
// BEFORE desugarJury strips the blocks (validation over the desugared map would
// see none — the jury: key is gone by the time ir.Validate ever runs). Returns
// diagnostics in the same shape ir.Validate uses.
func ValidateJury(wf *Workflow) []Diagnostic {
	var out []Diagnostic
	WalkNodes(wf.Graph, "", func(n Node, path string) {
		as, ok := n.(*AgentStep)
		if !ok || as.Jury == nil {
			return
		}
		j := as.Jury
		// AWF1072 (placement) is handled by a SEPARATE dedicated walk (checkJuryPlacement)
		// because "is this the terminal of a gate.evaluate" needs the parent-list context
		// WalkNodes does not expose. This closure handles only the three checks decidable
		// from the node alone.
		if as.OutputSchema == nil {
			out = append(out, Diagnostic{Path: path, Code: "AWF1069", Message: catalog["AWF1069"]})
			return // later checks need the schema
		}
		if !uniformKeys(j.Over) {
			out = append(out, Diagnostic{Path: path, Code: "AWF1070", Message: catalog["AWF1070"]})
		}
		if _, ok := JuryField(j, as.OutputSchema); !ok {
			out = append(out, Diagnostic{Path: path, Code: "AWF1071", Message: catalog["AWF1071"]})
		}
	})
	out = append(out, checkJuryPlacement(wf)...) // AWF1072
	return out
}

// JuryField resolves the boolean field quorum counts: the author's explicit
// Field, else the unique boolean property in output_schema. ok is false when
// Field is empty and there is not exactly one boolean field. Exported so
// loader.juryToMap can resolve the same default it validates against
// (loader/jury.go).
func JuryField(j *Jury, schema *JSONSchema) (string, bool) {
	if j.Field != "" {
		return j.Field, true
	}
	if schema == nil {
		return "", false
	}
	props, _ := (*schema)["properties"].(map[string]any)
	found := ""
	for name, spec := range props {
		m, ok := spec.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "boolean" {
			if found != "" {
				return "", false // ambiguous
			}
			found = name
		}
	}
	if found == "" {
		return "", false
	}
	return found, true
}

// checkJuryPlacement flags every jury: step that is NOT the last node of a gate's
// evaluate: list (AWF1072). A jury verdict IS the gate verdict, addressed via
// evaluate.<field>; the desugared map is anonymous (juryToMap), so anywhere else it
// is an unreferenceable dead computation. Walks every NodeList (mirroring
// loader.rewriteJuryList's coverage) so a jury nested in an if/loop/parallel/generate/
// map-body is caught too.
func checkJuryPlacement(wf *Workflow) []Diagnostic {
	var out []Diagnostic
	var walk func(list NodeList, parent string, exemptIdx int)
	// For a Gate.Evaluate list we pass the terminal index so the last element is
	// exempt; for every other list exemptIdx is -1 (no exemption).
	walk = func(list NodeList, parent string, exemptIdx int) {
		for i, n := range list {
			if as, ok := n.(*AgentStep); ok && as.Jury != nil && i != exemptIdx {
				out = append(out, Diagnostic{Path: PathFor(parent, "", as.ID, i), Code: "AWF1072", Message: catalog["AWF1072"]})
			}
			switch v := n.(type) {
			case *If:
				walk(v.Then, ChildPath(parent, "if", i, "then"), -1)
				walk(v.Else, ChildPath(parent, "if", i, "else"), -1)
			case *Loop:
				walk(v.Body, ChildPath(parent, "loop", i, "body"), -1)
			case *Try:
				walk(v.Do, ChildPath(parent, "try", i, "do"), -1)
				walk(v.Catch, ChildPath(parent, "try", i, "catch"), -1)
				walk(v.Finally, ChildPath(parent, "try", i, "finally"), -1)
			case *Parallel:
				walk(v.Children, PathFor(parent, "parallel", "", i), -1)
			case *Gate:
				walk(v.Generate, ChildPath(parent, "gate", i, "generate"), -1)
				walk(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), len(v.Evaluate)-1) // last node exempt
			case *Map:
				walk(v.Body, ChildPath(parent, "map", i, "body"), -1)
			case *Compose:
				walk(v.Body, ChildPath(parent, "compose", i, "body"), -1)
			}
		}
	}
	walk(wf.Graph, "", -1)
	return out
}

func uniformKeys(over []map[string]any) bool {
	if len(over) < 2 {
		return true
	}
	first := keySet(over[0])
	for _, item := range over[1:] {
		if !sameKeys(first, keySet(item)) {
			return false
		}
	}
	return true
}

func keySet(m map[string]any) map[string]struct{} {
	s := make(map[string]struct{}, len(m))
	for k := range m {
		s[k] = struct{}{}
	}
	return s
}

func sameKeys(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
