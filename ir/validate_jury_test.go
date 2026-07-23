package ir

import "testing"

func juryStep(j *Jury, schema *JSONSchema) *AgentStep {
	return &AgentStep{ID: "judge", Uses: "x/y", Container: "c", OutputSchema: schema, Jury: j}
}

func TestValidateJuryRequiresOutputSchema(t *testing.T) {
	q := Ratio("2")
	step := juryStep(&Jury{Over: []map[string]any{{"model": "a"}, {"model": "b"}}, Quorum: &q}, nil)
	errs := ValidateJury(&Workflow{Graph: NodeList{step}})
	assertOneError(t, errs, "AWF1069")
}

func TestValidateJuryUniformOverKeys(t *testing.T) {
	q := Ratio("2")
	schema := &JSONSchema{"type": "object", "properties": map[string]any{"accept": map[string]any{"type": "boolean"}}}
	step := juryStep(&Jury{
		Over:   []map[string]any{{"model": "a"}, {"model": "b", "effort": "high"}},
		Quorum: &q, Field: "accept",
	}, schema)
	errs := ValidateJury(&Workflow{Graph: NodeList{step}})
	assertOneError(t, errs, "AWF1070")
}

func TestValidateJuryFieldRequiredWhenAmbiguous(t *testing.T) {
	q := Ratio("2")
	schema := &JSONSchema{"type": "object", "properties": map[string]any{
		"accept": map[string]any{"type": "boolean"},
		"strict": map[string]any{"type": "boolean"},
	}}
	step := juryStep(&Jury{Over: []map[string]any{{"model": "a"}, {"model": "b"}}, Quorum: &q}, schema)
	errs := ValidateJury(&Workflow{Graph: NodeList{step}})
	assertOneError(t, errs, "AWF1071")
}

func TestValidateJuryValidPlacementNoError(t *testing.T) {
	q := Ratio("2")
	schema := &JSONSchema{"type": "object", "properties": map[string]any{"accept": map[string]any{"type": "boolean"}}}
	step := juryStep(&Jury{
		Over:   []map[string]any{{"model": "a"}, {"model": "b"}},
		Quorum: &q, Field: "accept",
	}, schema)
	gate := &Gate{
		Generate:    NodeList{&AgentStep{ID: "gen", Uses: "x/y", Container: "c"}},
		Evaluate:    NodeList{step},
		Until:       "{{ evaluate.accept }}",
		MaxAttempts: 3,
	}
	errs := ValidateJury(&Workflow{Graph: NodeList{gate}})
	assertNoErrorCode(t, errs, "AWF1072")
}

func TestValidateJuryRejectsNestedUnderIfInsideGateEvaluate(t *testing.T) {
	q := Ratio("2")
	schema := &JSONSchema{"type": "object", "properties": map[string]any{"accept": map[string]any{"type": "boolean"}}}
	step := juryStep(&Jury{
		Over:   []map[string]any{{"model": "a"}, {"model": "b"}},
		Quorum: &q, Field: "accept",
	}, schema)
	// The jury step is nested inside an if, which IS the last node of
	// gate.evaluate — but the jury step itself is not the evaluate list's
	// terminal element, only the if is. checkJuryPlacement's recursion must
	// still catch it (exemptIdx does not propagate into child NodeLists).
	gate := &Gate{
		Generate:    NodeList{&AgentStep{ID: "gen", Uses: "x/y", Container: "c"}},
		Evaluate:    NodeList{&If{Cond: "{{ true }}", Then: NodeList{step}}},
		Until:       "{{ evaluate.accept }}",
		MaxAttempts: 3,
	}
	errs := ValidateJury(&Workflow{Graph: NodeList{gate}})
	assertOneError(t, errs, "AWF1072")
}

func TestValidateJuryRejectsNonGatePlacement(t *testing.T) {
	q := Ratio("2")
	schema := &JSONSchema{"type": "object", "properties": map[string]any{"accept": map[string]any{"type": "boolean"}}}
	step := juryStep(&Jury{
		Over:   []map[string]any{{"model": "a"}, {"model": "b"}},
		Quorum: &q, Field: "accept",
	}, schema)
	errs := ValidateJury(&Workflow{Graph: NodeList{step}})
	assertOneError(t, errs, "AWF1072")
}
