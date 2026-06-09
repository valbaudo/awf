package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

func namedAggregateReduceSchema() *JSONSchema {
	return &JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"total"},
		"properties":           map[string]any{"total": map[string]any{"type": "integer"}},
	}
}

func namedAggregateMap(id string, reduce *Reduce, finalSchema *JSONSchema) *Map {
	return &Map{
		ID:          id,
		Over:        Expr("{{ step.find_urls.urls }}"),
		As:          "u",
		Container:   "c",
		Concurrency: 1,
		Body: NodeList{
			&AgentStep{
				ID:           "scan",
				Container:    "c",
				Uses:         "anthropic/claude-code",
				With:         RawConfig{"prompt": "scan {{ u }}"},
				OutputSchema: finalSchema,
			},
		},
		Reduce: reduce,
	}
}

func namedAggregateRunReduce() *Reduce {
	return &Reduce{
		Run:          "true",
		Container:    "c",
		OutputSchema: namedAggregateReduceSchema(),
		OutputFiles:  OutputFiles{{Name: "files", Path: "/out/files.jsonl"}},
	}
}

func namedAggregateWorkflow(graph NodeList) *LoadedDefinition {
	return makeLD(&Workflow{
		ID:         "named-agg",
		Version:    1,
		Containers: aggContainer(),
		Graph:      graph,
	})
}

func TestMapIDUnmarshalMarshal(t *testing.T) {
	const src = `[{"map":{"id":"version_universe","over":"{{ input.items }}","as":"item","container":"lab","concurrency":1,"body":[{"id":"collect","container":"lab","run":"true"}]}}]`
	var nodes NodeList
	if err := json.Unmarshal([]byte(src), &nodes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, ok := nodes[0].(*Map)
	if !ok {
		t.Fatalf("node[0] is %T, want *Map", nodes[0])
	}
	if m.ID != "version_universe" {
		t.Fatalf("Map.ID = %q, want version_universe", m.ID)
	}
	b, err := json.Marshal(nodes)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"id":"version_universe"`) {
		t.Fatalf("marshaled map omitted id: %s", b)
	}
}

func TestStructuralMapIDUsesStepIDRules(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("bad/id", nil, aggScanSchema()),
	})
	assertErrorAt(t, Validate(ld), "AWF1020", "map[1]")
}

func TestStructuralDuplicateStepAndMapIDRejected(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		&CodeStep{ID: "version_universe", Container: "c", Run: "true"},
		namedAggregateMap("version_universe", nil, aggScanSchema()),
	})
	assertErrorAt(t, Validate(ld), "AWF1004", "map[2]")
}

func TestStructuralDuplicateMapIDsRejected(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", nil, aggScanSchema()),
		&Map{
			ID:          "version_universe",
			Over:        Expr("{{ step.find_urls.urls }}"),
			As:          "v",
			Container:   "c",
			Concurrency: 1,
			Body: NodeList{
				&CodeStep{ID: "collect", Container: "c", Run: `echo '{"finding":"x","index":0}' > "$AWF_OUTPUT"`, OutputSchema: aggScanSchema()},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF1004", "map[2]")
}

func TestMapCompactProducerRequiresTypedFinalBodyStep(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", nil, nil),
	})
	assertErrorAt(t, Validate(ld), "AWF5009", "map[1]")
}

func TestMapCompactProducerRejectsFinalControlNode(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		&Map{
			ID:          "version_universe",
			Over:        Expr("{{ step.find_urls.urls }}"),
			As:          "u",
			Container:   "c",
			Concurrency: 1,
			Body: NodeList{
				&If{Cond: Expr("{{ true }}"), Then: NodeList{&CodeStep{ID: "collect", Container: "c", Run: "true", OutputSchema: aggScanSchema()}}},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5009", "map[1]")
}

func TestNamedCompactMapInsideLoopRejected(t *testing.T) {
	maxIters := 1
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		&Loop{MaxIters: &maxIters, Body: NodeList{
			namedAggregateMap("version_universe", nil, aggScanSchema()),
		}},
	})
	assertErrorAt(t, Validate(ld), "AWF5011", "loop[1].body.map[0]")
}

func TestNamedReducedMapInsideGateRejected(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		&Gate{
			MaxAttempts: 1,
			Until:       Expr("{{ evaluate.ok }}"),
			Generate: NodeList{
				namedAggregateMap("version_universe", namedAggregateRunReduce(), aggScanSchema()),
			},
			Evaluate: NodeList{
				&AgentStep{ID: "judge", Uses: "anthropic/claude-code", OutputSchema: &JSONSchema{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5011", "gate[1].generate.map[0]")
}

func TestNamedReducedMapInsideNestedMapRejected(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		&Map{
			Over:        Expr("{{ step.find_urls.urls }}"),
			As:          "u",
			Container:   "c",
			Concurrency: 1,
			Body: NodeList{
				namedAggregateMap("version_universe", namedAggregateRunReduce(), aggScanSchema()),
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5011", "map[1].body.map[0]")
}

func TestNamedReducedMapFieldRefAccepted(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", namedAggregateRunReduce(), aggScanSchema()),
		&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.version_universe.total }}"},
	})
	diags := Validate(ld)
	assertNoCode(t, diags, "AWF3001")
	assertNoCode(t, diags, "AWF5004")
}

func TestNamedReducedMapArtifactRefAccepted(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", namedAggregateRunReduce(), aggScanSchema()),
		&CodeStep{ID: "after", Container: "c", Run: "true", InputFiles: map[string]string{
			"/work/files.jsonl": "step.version_universe.files.files",
		}},
	})
	assertNoCode(t, Validate(ld), "AWF3007")
}

func TestNamedMapArtifactRefWithoutReduceRejected(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", nil, aggScanSchema()),
		&CodeStep{ID: "after", Container: "c", Run: "true", InputFiles: map[string]string{
			"/work/files.jsonl": "step.version_universe.files.files",
		}},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "after")
}

func TestNamedCompactMapWholeOutputRefIntoOverAccepted(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", nil, aggScanSchema()),
		&Map{Over: Expr("{{ step.version_universe }}"), As: "finding", Container: "c", Concurrency: 1, Body: NodeList{
			&CodeStep{ID: "consume", Container: "c", Run: "true"},
		}},
	})
	diags := Validate(ld)
	assertNoCode(t, diags, "AWF3001")
	assertNoCode(t, diags, "AWF5004")
}

func TestNamedCompactMapFieldRefIntoOverAccepted(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", nil, aggScanSchema()),
		&Map{Over: Expr("{{ step.version_universe.finding }}"), As: "finding", Container: "c", Concurrency: 1, Body: NodeList{
			&CodeStep{ID: "consume", Container: "c", Run: "true"},
		}},
	})
	diags := Validate(ld)
	assertNoCode(t, diags, "AWF3001")
	assertNoCode(t, diags, "AWF5004")
}

func TestNamedCompactMapRefOutsideOverRejected(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", nil, aggScanSchema()),
		&CodeStep{ID: "after", Container: "c", Run: "echo {{ step.version_universe.finding }}"},
	})
	assertErrorAt(t, Validate(ld), "AWF5004", "after.run")
}

func TestNamedCompactMapSelfOverRejected(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		&Map{
			ID:          "version_universe",
			Over:        Expr("{{ step.version_universe }}"),
			As:          "u",
			Container:   "c",
			Concurrency: 1,
			Body: NodeList{
				&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code", OutputSchema: aggScanSchema()},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5010", "map[0].over")
}

func TestNamedReducedMapSelfOverRejected(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		&Map{
			ID:          "version_universe",
			Over:        Expr("{{ step.version_universe.total }}"),
			As:          "u",
			Container:   "c",
			Concurrency: 1,
			Body: NodeList{
				&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code", OutputSchema: aggScanSchema()},
			},
			Reduce: namedAggregateRunReduce(),
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5010", "map[0].over")
}

func TestMapBodyStepSelfOverRejectedWithoutReduce(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		&Map{
			Over:        Expr("{{ step.scan.finding }}"),
			As:          "u",
			Container:   "c",
			Concurrency: 1,
			Body: NodeList{
				&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code", OutputSchema: aggScanSchema()},
			},
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5002", "map[0].over")
}

func TestMapBodyStepSelfOverRejectedWithReduce(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		&Map{
			Over:        Expr("{{ step.scan.total }}"),
			As:          "u",
			Container:   "c",
			Concurrency: 1,
			Body: NodeList{
				&AgentStep{ID: "scan", Container: "c", Uses: "anthropic/claude-code", OutputSchema: namedAggregateReduceSchema()},
			},
			Reduce: namedAggregateRunReduce(),
		},
	})
	assertErrorAt(t, Validate(ld), "AWF5002", "map[0].over")
}

func TestNamedReducedMapBodyArtifactSelfRefRejected(t *testing.T) {
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		&Map{
			ID:          "version_universe",
			Over:        Expr("{{ step.find_urls.urls }}"),
			As:          "u",
			Container:   "c",
			Concurrency: 1,
			Body: NodeList{
				&CodeStep{ID: "consume", Container: "c", Run: "true", InputFiles: map[string]string{
					"/work/files.jsonl": "step.version_universe.files.files",
				}},
			},
			Reduce: namedAggregateRunReduce(),
		},
	})
	assertErrorAt(t, Validate(ld), "AWF3007", "map[1].body.consume")
}

func TestNamedReducedMapReducerRunSelfRefRejected(t *testing.T) {
	r := namedAggregateRunReduce()
	r.Run = "echo {{ step.version_universe.total }}"
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", r, aggScanSchema()),
	})
	assertErrorAt(t, Validate(ld), "AWF5010", "map[1].reduce.run")
}

func TestNamedReducedMapReducerOutputFilePathSelfRefRejected(t *testing.T) {
	r := namedAggregateRunReduce()
	r.OutputFiles = OutputFiles{{Name: "files", Path: "/out/{{ step.version_universe.total }}.jsonl"}}
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", r, aggScanSchema()),
	})
	assertErrorAt(t, Validate(ld), "AWF5010", "map[1].reduce.output_files.files")
}

func TestReducedMapReducerRunBodyStepRefAccepted(t *testing.T) {
	r := namedAggregateRunReduce()
	r.Run = "echo {{ step.scan.finding }}"
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", r, aggScanSchema()),
	})
	assertNoCode(t, Validate(ld), "AWF5002")
	assertNoCode(t, Validate(ld), "AWF5004")
	assertNoErrorCode(t, Validate(ld), "AWF3001")
}

func TestReducedMapReducerOutputFilePathBodyStepRefAccepted(t *testing.T) {
	r := namedAggregateRunReduce()
	r.OutputFiles = OutputFiles{{Name: "files", Path: "/out/{{ step.scan.finding }}.jsonl"}}
	ld := namedAggregateWorkflow(NodeList{
		aggFindURLs(),
		namedAggregateMap("version_universe", r, aggScanSchema()),
	})
	assertNoCode(t, Validate(ld), "AWF5002")
	assertNoCode(t, Validate(ld), "AWF5004")
	assertNoErrorCode(t, Validate(ld), "AWF3001")
}
