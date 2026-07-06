package ir

import (
	"encoding/json"
	"testing"
	"time"
)

func durPtr(d time.Duration) *Duration { x := Duration(d); return &x }

func TestTimeout_UnmarshalScalarString(t *testing.T) {
	var to Timeout
	if err := json.Unmarshal([]byte(`"45m"`), &to); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if to.Wall == nil || time.Duration(*to.Wall) != 45*time.Minute {
		t.Fatalf("Wall = %v, want 45m", to.Wall)
	}
	if to.Idle != nil {
		t.Fatalf("Idle = %v, want nil", to.Idle)
	}
}

func TestTimeout_UnmarshalMap(t *testing.T) {
	var to Timeout
	if err := json.Unmarshal([]byte(`{"wall":"45m","idle":"5m"}`), &to); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if to.Wall == nil || time.Duration(*to.Wall) != 45*time.Minute {
		t.Fatalf("Wall = %v, want 45m", to.Wall)
	}
	if to.Idle == nil || time.Duration(*to.Idle) != 5*time.Minute {
		t.Fatalf("Idle = %v, want 5m", to.Idle)
	}
}

func TestTimeout_UnmarshalIdleOnly(t *testing.T) {
	var to Timeout
	if err := json.Unmarshal([]byte(`{"idle":"5m"}`), &to); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if to.Wall != nil {
		t.Fatalf("Wall = %v, want nil", to.Wall)
	}
	if to.Idle == nil || time.Duration(*to.Idle) != 5*time.Minute {
		t.Fatalf("Idle = %v, want 5m", to.Idle)
	}
}

func TestTimeout_UnmarshalUnknownSubkeyRejected(t *testing.T) {
	var to Timeout
	if err := json.Unmarshal([]byte(`{"wall":"45m","grace":"5m"}`), &to); err == nil {
		t.Fatal("expected error for unknown sub-key under timeout, got nil")
	}
}

// TestTimeout_MarshalScalarIsDigestStable is the load-bearing back-compat test:
// a wall-only Timeout must marshal to the exact bytes a *Duration field would, so
// a scalar-form workflow's whole-workflow JCS digest is byte-identical to before
// the map form existed.
func TestTimeout_MarshalScalarIsDigestStable(t *testing.T) {
	to := Timeout{Wall: durPtr(45 * time.Minute)}
	got, err := json.Marshal(&to)
	if err != nil {
		t.Fatalf("marshal timeout: %v", err)
	}
	want, err := json.Marshal(durPtr(45 * time.Minute)) // what the old *Duration field emitted
	if err != nil {
		t.Fatalf("marshal duration: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("wall-only Timeout marshaled %s, want %s (digest stability)", got, want)
	}
}

func TestTimeout_MapRoundTrip(t *testing.T) {
	to := Timeout{Wall: durPtr(45 * time.Minute), Idle: durPtr(5 * time.Minute)}
	b, err := json.Marshal(&to)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Timeout
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Wall == nil || *back.Wall != *to.Wall || back.Idle == nil || *back.Idle != *to.Idle {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}
