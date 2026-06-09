package ir

import (
	"encoding/json"
	"testing"
)

func TestParsePruneKeep(t *testing.T) {
	cases := []struct {
		raw     string
		wantK   int
		wantErr bool
	}{
		{"top(5)", 5, false},
		{"top(1)", 1, false},
		{"  top( 3 )  ", 3, false}, // surrounding + inner whitespace trimmed
		{"top(0)", 0, true},        // k must be positive
		{"top(-1)", 0, true},       // k must be positive
		{"top()", 0, true},         // missing k
		{"top(5", 0, true},         // unbalanced paren
		{"keep(5)", 0, true},       // wrong head
		{"top(abc)", 0, true},      // non-int k
	}
	for _, tc := range cases {
		got, err := ParsePruneKeep(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParsePruneKeep(%q): want error, got K=%d nil", tc.raw, got.K)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePruneKeep(%q): unexpected error: %v", tc.raw, err)
			continue
		}
		if got.K != tc.wantK {
			t.Errorf("ParsePruneKeep(%q): K=%d, want %d", tc.raw, got.K, tc.wantK)
		}
	}
}

func TestMapUnmarshalPruneKeep(t *testing.T) {
	raw := []byte(`{"map":{"over":"input.items","as":"item","container":"lab","body":[{"id":"b","run":"x"}],"prune":{"score":"s","keep":"top(3)"}}}`)
	var nl NodeList
	if err := json.Unmarshal([]byte("["+string(raw)+"]"), &nl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, ok := nl[0].(*Map)
	if !ok {
		t.Fatalf("node 0 is %T, want *Map", nl[0])
	}
	if m.Prune == nil {
		t.Fatal("Map.Prune is nil, want non-nil")
	}
	if m.Prune.Score != "s" {
		t.Errorf("Prune.Score = %q, want %q", m.Prune.Score, "s")
	}
	if m.Prune.Keep == nil {
		t.Fatal("Prune.Keep is nil, want top(3)")
	}
	if m.Prune.Keep.K != 3 {
		t.Errorf("Prune.Keep.K = %d, want 3", m.Prune.Keep.K)
	}
	if m.Prune.StopWhen != "" {
		t.Errorf("Prune.StopWhen = %q, want empty", m.Prune.StopWhen)
	}
}

func TestMapUnmarshalPruneStopWhen(t *testing.T) {
	raw := []byte(`{"map":{"over":"input.items","as":"item","container":"lab","body":[{"id":"b","run":"x"}],"prune":{"score":"s","stop_when":"{{ best.score >= 0.9 }}"}}}`)
	var nl NodeList
	if err := json.Unmarshal([]byte("["+string(raw)+"]"), &nl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, ok := nl[0].(*Map)
	if !ok {
		t.Fatalf("node 0 is %T, want *Map", nl[0])
	}
	if m.Prune == nil {
		t.Fatal("Map.Prune is nil, want non-nil")
	}
	if m.Prune.StopWhen != "{{ best.score >= 0.9 }}" {
		t.Errorf("Prune.StopWhen = %q, want the bool expr", m.Prune.StopWhen)
	}
	if m.Prune.Keep != nil {
		t.Errorf("Prune.Keep = %+v, want nil", m.Prune.Keep)
	}
}

func TestMapPruneRoundTrip(t *testing.T) {
	// keep form
	keep := &Map{
		Over: "input.items", As: "item", Container: "lab",
		Body:  NodeList{&CodeStep{ID: "b", Run: "x"}},
		Prune: &Prune{Score: "s", Keep: &PruneKeep{K: 4}},
	}
	rtKeep := roundTripMap(t, keep)
	if rtKeep.Prune == nil || rtKeep.Prune.Score != "s" || rtKeep.Prune.Keep == nil || rtKeep.Prune.Keep.K != 4 {
		t.Fatalf("keep round-trip lost the policy: %+v", rtKeep.Prune)
	}
	if rtKeep.Prune.StopWhen != "" {
		t.Errorf("keep round-trip leaked StopWhen: %q", rtKeep.Prune.StopWhen)
	}

	// stop_when form
	stop := &Map{
		Over: "input.items", As: "item", Container: "lab",
		Body:  NodeList{&CodeStep{ID: "b", Run: "x"}},
		Prune: &Prune{Score: "s", StopWhen: "{{ best.score >= 0.9 }}"},
	}
	rtStop := roundTripMap(t, stop)
	if rtStop.Prune == nil || rtStop.Prune.Score != "s" || rtStop.Prune.StopWhen != "{{ best.score >= 0.9 }}" {
		t.Fatalf("stop_when round-trip lost the policy: %+v", rtStop.Prune)
	}
	if rtStop.Prune.Keep != nil {
		t.Errorf("stop_when round-trip leaked Keep: %+v", rtStop.Prune.Keep)
	}
}

// roundTripMap marshals a *Map node through a NodeList and unmarshals it back.
func roundTripMap(t *testing.T, m *Map) *Map {
	t.Helper()
	b, err := json.Marshal(NodeList{m})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var nl NodeList
	if err := json.Unmarshal(b, &nl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, ok := nl[0].(*Map)
	if !ok {
		t.Fatalf("round-trip node is %T, want *Map", nl[0])
	}
	return out
}

func TestDigestFoldsPrune(t *testing.T) {
	// A prune: clause on a map is part of the definition: declaring it changes
	// the digest (so resume hard-errors on a changed policy), while a nil Prune
	// leaves the digest byte-identical (omitempty backwards-compat).
	withPrune := sampleWorkflow()
	withPrune.Graph = append(withPrune.Graph,
		&Map{Over: "input.items", As: "item", Container: "lab",
			Body:  NodeList{&CodeStep{ID: "b", Run: "x"}},
			Prune: &Prune{Score: "s", Keep: &PruneKeep{K: 2}}},
	)
	dPrune, err := withPrune.ComputeDigest(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	noPrune := sampleWorkflow()
	noPrune.Graph = append(noPrune.Graph,
		&Map{Over: "input.items", As: "item", Container: "lab",
			Body: NodeList{&CodeStep{ID: "b", Run: "x"}}},
	)
	dNo, err := noPrune.ComputeDigest(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dPrune == dNo {
		t.Fatalf("prune: did not change the digest (got %s for both)", dPrune)
	}
	// A different policy yields a different digest (the policy is pinned).
	withStop := sampleWorkflow()
	withStop.Graph = append(withStop.Graph,
		&Map{Over: "input.items", As: "item", Container: "lab",
			Body:  NodeList{&CodeStep{ID: "b", Run: "x"}},
			Prune: &Prune{Score: "s", StopWhen: "{{ best.score >= 0.9 }}"}},
	)
	dStop, err := withStop.ComputeDigest(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dStop == dPrune {
		t.Fatalf("different prune policies hashed equal: %s", dPrune)
	}
	// The k of keep: top(k) folds in too — PruneKeep.K has no json tag (its custom
	// marshaler owns the "top(<k>)" wire form), so this guards that the k actually
	// reaches the hash: top(2) and top(3) must differ.
	withK3 := sampleWorkflow()
	withK3.Graph = append(withK3.Graph,
		&Map{Over: "input.items", As: "item", Container: "lab",
			Body:  NodeList{&CodeStep{ID: "b", Run: "x"}},
			Prune: &Prune{Score: "s", Keep: &PruneKeep{K: 3}}},
	)
	dK3, err := withK3.ComputeDigest(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dK3 == dPrune {
		t.Fatalf("keep: top(k) k value did not fold into the digest (top(2)==top(3): %s)", dPrune)
	}
}

func TestLastStepID(t *testing.T) {
	// LastStepID returns the id of the body's LAST node when it is a code/agent
	// step (the producer of the prune score), or ("", false) otherwise. The
	// engine reads the score from Completed[itemPath + "." + id].
	cases := []struct {
		name   string
		body   NodeList
		wantID string
		wantOK bool
	}{
		{"single code step", NodeList{&CodeStep{ID: "hyp", Run: "x"}}, "hyp", true},
		{"single agent step", NodeList{&AgentStep{ID: "gen", Uses: "claude"}}, "gen", true},
		{"last of several", NodeList{&CodeStep{ID: "a", Run: "x"}, &CodeStep{ID: "score", Run: "y"}}, "score", true},
		{"empty body", NodeList{}, "", false},
		{"last node is control flow", NodeList{&CodeStep{ID: "a", Run: "x"}, &If{Cond: "{{ true }}"}}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := LastStepID(tc.body)
			if id != tc.wantID || ok != tc.wantOK {
				t.Errorf("LastStepID = (%q, %v), want (%q, %v)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}
