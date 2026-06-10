package ir

import (
	"reflect"
	"testing"
)

// TestWalkNodesVisitsEveryNodeWithCanonicalPaths pins the pre-order traversal
// order and the exact path strings (incl. the Parallel-no-branch-label quirk and
// the gate generate/evaluate self+child paths) that ir.PathFor/ir.ChildPath
// produce. This is the load-bearing contract every migrated caller depends on.
func TestWalkNodesVisitsEveryNodeWithCanonicalPaths(t *testing.T) {
	graph := NodeList{
		&CodeStep{ID: "triage"}, // index 0
		&Loop{ // index 1
			Body: NodeList{
				&CodeStep{ID: "echo"}, // loop[1].body.echo
				&If{ // loop[1].body.if[1]
					Then: NodeList{&CodeStep{ID: "deep"}},
					Else: NodeList{&CodeStep{ID: "deep_else"}},
				},
			},
		},
		&Parallel{Children: NodeList{ // index 2
			&CodeStep{ID: "p0"},
		}},
		&Gate{ // index 3
			Generate: NodeList{&CodeStep{ID: "gen"}},
			Evaluate: NodeList{&CodeStep{ID: "judge"}},
		},
		&Map{Body: NodeList{&CodeStep{ID: "item"}}}, // index 4
		&Try{ // index 5
			Do:      NodeList{&CodeStep{ID: "t_do"}},
			Catch:   NodeList{&CodeStep{ID: "t_catch"}},
			Finally: NodeList{&CodeStep{ID: "t_fin"}},
		},
		&Compose{ // index 6
			As: "lab", From: "step.gen.files.compose", Service: "web",
			Body: NodeList{&CodeStep{ID: "smoke"}},
		},
		&AgentStep{ID: "a0"},  // index 7
		&SignalStep{ID: "s0"}, // index 8
		&CallStep{ID: "c0"},   // index 9
		&Skip{Reason: "done"}, // index 10 — must NOT be visited (no skip[N])
	}

	type visited struct {
		kind string
		path string
	}
	var got []visited
	WalkNodes(graph, "", func(n Node, path string) {
		got = append(got, visited{kind: reflect.TypeOf(n).String(), path: path})
	})

	// Note the absence of any *ir.Skip / "skip[8]" entry: Skip is deliberately not
	// visited (no addressable identity, not in the §8 grammar). DeepEqual fails if
	// a skip[N] entry ever appears.
	want := []visited{
		{"*ir.CodeStep", "triage"},
		{"*ir.Loop", "loop[1]"},
		{"*ir.CodeStep", "loop[1].body.echo"},
		{"*ir.If", "loop[1].body.if[1]"},
		{"*ir.CodeStep", "loop[1].body.if[1].then.deep"},
		{"*ir.CodeStep", "loop[1].body.if[1].else.deep_else"},
		{"*ir.Parallel", "parallel[2]"},
		{"*ir.CodeStep", "parallel[2].p0"}, // bare parallel[2], NO branch label
		{"*ir.Gate", "gate[3]"},
		{"*ir.CodeStep", "gate[3].generate.gen"},
		{"*ir.CodeStep", "gate[3].evaluate.judge"},
		{"*ir.Map", "map[4]"},
		{"*ir.CodeStep", "map[4].body.item"},
		{"*ir.Try", "try[5]"},
		{"*ir.CodeStep", "try[5].do.t_do"},
		{"*ir.CodeStep", "try[5].catch.t_catch"},
		{"*ir.CodeStep", "try[5].finally.t_fin"},
		{"*ir.Compose", "compose[6]"},
		{"*ir.CodeStep", "compose[6].body.smoke"},
		{"*ir.AgentStep", "a0"},
		{"*ir.SignalStep", "s0"},
		{"*ir.CallStep", "c0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("traversal mismatch:\n got=%v\nwant=%v", got, want)
	}
}
