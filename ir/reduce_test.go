package ir

import (
	"encoding/json"
	"testing"
)

// TestReduceUnmarshalQuorum decodes the quorum form of reduce: on a map.
func TestReduceUnmarshalQuorum(t *testing.T) {
	const src = `{"map":{"over":"input.items","as":"item","container":"lab",` +
		`"body":[{"id":"b","run":"x"}],` +
		`"reduce":{"quorum":2,"field":"vulnerable"}}}`
	var n NodeList
	if err := json.Unmarshal([]byte("["+src+"]"), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, ok := n[0].(*Map)
	if !ok {
		t.Fatalf("node[0] is %T, want *Map", n[0])
	}
	if m.Reduce == nil {
		t.Fatal("Map.Reduce is nil")
	}
	if !m.Reduce.IsQuorum() {
		t.Errorf("IsQuorum() = false, want true")
	}
	if m.Reduce.IsRun() {
		t.Errorf("IsRun() = true, want false")
	}
	if m.Reduce.Quorum == nil || m.Reduce.Quorum.String() != "2" {
		t.Errorf("Quorum = %v, want 2", m.Reduce.Quorum)
	}
	if m.Reduce.Field != "vulnerable" {
		t.Errorf("Field = %q, want vulnerable", m.Reduce.Field)
	}
}

// TestReduceUnmarshalRun decodes the author run: reducer form.
func TestReduceUnmarshalRun(t *testing.T) {
	const src = `{"map":{"over":"input.items","as":"item","container":"lab",` +
		`"body":[{"id":"b","run":"x"}],` +
		`"reduce":{"run":"./m.sh","container":"agg",` +
		`"output_schema":{"type":"object"},` +
		`"output_files":{"csv":"/out/x.csv"}}}}`
	var n NodeList
	if err := json.Unmarshal([]byte("["+src+"]"), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, ok := n[0].(*Map)
	if !ok {
		t.Fatalf("node[0] is %T, want *Map", n[0])
	}
	if m.Reduce == nil {
		t.Fatal("Map.Reduce is nil")
	}
	if !m.Reduce.IsRun() {
		t.Errorf("IsRun() = false, want true")
	}
	if m.Reduce.IsQuorum() {
		t.Errorf("IsQuorum() = true, want false")
	}
	if m.Reduce.Run != "./m.sh" {
		t.Errorf("Run = %q, want ./m.sh", m.Reduce.Run)
	}
	if m.Reduce.Container != "agg" {
		t.Errorf("Container = %q, want agg", m.Reduce.Container)
	}
	if m.Reduce.OutputSchema == nil {
		t.Error("OutputSchema is nil")
	}
	path, ok := m.Reduce.OutputFiles.PathForName("csv")
	if !ok || path != "/out/x.csv" {
		t.Errorf("OutputFiles[csv] = %q,%v, want /out/x.csv,true", path, ok)
	}
}

// TestReduceHelpersNilSafe confirms the form helpers tolerate a nil receiver.
func TestReduceHelpersNilSafe(t *testing.T) {
	var r *Reduce
	if r.IsQuorum() {
		t.Error("nil.IsQuorum() = true, want false")
	}
	if r.IsRun() {
		t.Error("nil.IsRun() = true, want false")
	}
}
