package ir

import (
	"encoding/json"
	"testing"
)

// A map with a runtime-resolved image: round-trips through JSON with the
// template text preserved (P6a). Guards the new field's (un)marshal wiring.
func TestMapImageRoundTrips(t *testing.T) {
	src := `{"map":{"over":"{{ input.items }}","as":"v","container":"lab","image":"{{ v.image }}","concurrency":2,"body":[]}}`
	var nl NodeList
	if err := json.Unmarshal([]byte(`[`+src+`]`), &nl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, ok := nl[0].(*Map)
	if !ok {
		t.Fatalf("node[0] is %T, want *Map", nl[0])
	}
	if m.Image != "{{ v.image }}" {
		t.Errorf("Map.Image = %q, want %q", m.Image, "{{ v.image }}")
	}
	out, err := json.Marshal(nl[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(out); got != src {
		t.Errorf("re-marshal = %s, want %s", got, src)
	}
}
