package ir

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDurationRoundTrip(t *testing.T) {
	// Integer ns marshal: bare integer, no quotes.
	d := Duration(5 * time.Second)
	b, err := json.Marshal(&d)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "5000000000" {
		t.Fatalf("marshal: got %s, want 5000000000", b)
	}
	// Round-trip restores the value.
	var d2 Duration
	if err := json.Unmarshal(b, &d2); err != nil {
		t.Fatal(err)
	}
	if d2 != d {
		t.Fatalf("round-trip: got %v, want %v", time.Duration(d2), time.Duration(d))
	}
}

func TestDurationParsesGoString(t *testing.T) {
	// Authors writing `timeout: 24h` in YAML reach decode as a quoted string; accept it.
	var d Duration
	if err := json.Unmarshal([]byte(`"5s"`), &d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(d) != 5*time.Second {
		t.Fatalf("got %v, want 5s", time.Duration(d))
	}
}
