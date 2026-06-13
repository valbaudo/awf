package ir

import (
	"encoding/json"
	"testing"
)

func TestToolImplOmitsEmptyFields(t *testing.T) {
	// An id-less impl with only run+container must NOT serialize an empty "id"
	// (the reason it is not a reused CodeStep) — and must omit empty optionals.
	impl := ToolImpl{Run: "true", Container: "lab"}
	b, err := json.Marshal(impl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	want := `{"run":"true","container":"lab"}`
	if got != want {
		t.Fatalf("ToolImpl JSON = %s, want %s", got, want)
	}
}

func TestToolRoundTrip(t *testing.T) {
	in := Tool{
		Description: "Validate an IBAN",
		InputSchema: &JSONSchema{"type": "object"},
		Impl:        ToolImpl{Run: "./validate --args-file {{ args_file }}", Container: "fin"},
	}
	b, _ := json.Marshal(in)
	var out Tool
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Description != in.Description || out.Impl.Run != in.Impl.Run || out.Impl.Container != "fin" {
		t.Fatalf("round-trip lost data: %+v", out)
	}
}
