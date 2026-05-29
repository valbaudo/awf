package droid

import "testing"

// TestLastEnvelope covers lastEnvelope's branches directly (it is unexported):
// the clean single-line case, the bottom-up multi-line fallback (a stray stdout
// line before the JSON envelope), the empty-object Type=="" guard, and the
// no-JSON case. droid -o json normally emits one clean line, so the fallback is
// defensive — pin it so a future edit can't silently break it.
func TestLastEnvelope(t *testing.T) {
	// Clean single line (with trailing newline).
	if _, env := lastEnvelope([]byte(`{"type":"result","subtype":"success","result":"ok"}` + "\n")); env == nil || env.Result != "ok" {
		t.Fatalf("single-line: env = %+v", env)
	}

	// Stray non-JSON line before the envelope → whole-buffer parse fails, the
	// bottom-up line scan picks the last parseable line.
	raw, env := lastEnvelope([]byte("npm warn deprecated foo\n" + `{"type":"result","subtype":"success","result":"hi"}` + "\n"))
	if env == nil || env.Result != "hi" {
		t.Fatalf("multi-line fallback: env = %+v", env)
	}
	if string(raw) != `{"type":"result","subtype":"success","result":"hi"}` {
		t.Errorf("multi-line raw = %q, want just the envelope line", raw)
	}

	// Empty object {} parses as JSON but has Type=="" → treated as no envelope.
	if _, env := lastEnvelope([]byte("{}")); env != nil {
		t.Errorf("empty object should be treated as no envelope, got %+v", env)
	}

	// No parseable JSON anywhere → nil.
	if _, env := lastEnvelope([]byte("total garbage\nmore garbage")); env != nil {
		t.Errorf("garbage should yield nil envelope, got %+v", env)
	}
}
