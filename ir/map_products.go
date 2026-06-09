package ir

// MapCompactProducer reports the final typed body-step product for a map without
// reduce:. Named compact map products expose that final step's typed outputs as a
// compact array for downstream map over: expressions.
func MapCompactProducer(m *Map) (suffix string, schema *JSONSchema, ok bool) {
	if m == nil || len(m.Body) == 0 {
		return "", nil, false
	}
	switch s := m.Body[len(m.Body)-1].(type) {
	case *CodeStep:
		return s.ID, s.OutputSchema, s.OutputSchema != nil
	case *AgentStep:
		return s.ID, s.OutputSchema, s.OutputSchema != nil
	case *SignalStep:
		return s.ID, s.OutputSchema, s.OutputSchema != nil
	default:
		return "", nil, false
	}
}

// MapProductShape reports whether a map at static mapPath is in the same v1
// single-map envelope that aggregate refs can resolve: exactly one map boundary,
// no gate or loop multiplicity.
func MapProductShape(mapPath string) bool {
	_, _, ok := SingleMapBodyShape(mapPath + ".body.__product__")
	return ok
}
