package agent

import (
	"strings"
	"testing"
)

func TestDisplaySanitizeStripsTerminalControls(t *testing.T) {
	in := []byte("hi\x1b[31m red\x1b[0m \x1b]52;c;SECRET\a clip\x1bPignored\x1b\\ end\x00\x7f")
	got := SanitizeDisplayBytes(in)
	want := "hi red  clip end"
	if got != want {
		t.Fatalf("SanitizeDisplayBytes = %q, want %q", got, want)
	}
}

func TestDisplaySanitizeStripsC1StringControls(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want string
	}{
		{name: "raw OSC", in: []byte("ok \x9d52;c;SECRET\a done"), want: "ok  done"},
		{name: "raw DCS", in: []byte("ok \x90SECRET\x9c done"), want: "ok  done"},
		{name: "raw PM", in: []byte("ok \x9eSECRET\x9c done"), want: "ok  done"},
		{name: "raw APC", in: []byte("ok \x9fSECRET\x9c done"), want: "ok  done"},
		{name: "raw SOS", in: []byte("ok \x98SECRET\x9c done"), want: "ok  done"},
		{name: "utf8 OSC", in: []byte("ok \u009d52;c;SECRET\a done"), want: "ok  done"},
		{name: "utf8 DCS/ST", in: []byte("ok \u0090SECRET\u009c done"), want: "ok  done"},
		{name: "utf8 PM/ST", in: []byte("ok \u009eSECRET\u009c done"), want: "ok  done"},
		{name: "utf8 APC/ST", in: []byte("ok \u009fSECRET\u009c done"), want: "ok  done"},
		{name: "utf8 SOS/ST", in: []byte("ok \u0098SECRET\u009c done"), want: "ok  done"},
		{name: "utf8 CSI", in: []byte("ok \u009b31mred done"), want: "ok red done"},
		{name: "alternate screen", in: []byte("ok\x1b[?1049h hidden\x1b[?1049l done"), want: "ok hidden done"},
		{name: "cursor move", in: []byte("ok\x1b[2J\x1b[H done"), want: "ok done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeDisplayBytes(tc.in); got != tc.want {
				t.Fatalf("SanitizeDisplayBytes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDisplaySanitizePreservesNewlineAndTab(t *testing.T) {
	got := SanitizeDisplayText("line 1\nline\t2\rline 3")
	want := "line 1\nline\t2line 3"
	if got != want {
		t.Fatalf("SanitizeDisplayText = %q, want %q", got, want)
	}
}

func TestDisplaySanitizeDropsInvalidUTF8(t *testing.T) {
	got := SanitizeDisplayBytes([]byte{'o', 0xff, 'k'})
	if got != "ok" {
		t.Fatalf("SanitizeDisplayBytes invalid UTF-8 = %q, want %q", got, "ok")
	}
}

func TestRedactDisplayTextKnownSecretShapes(t *testing.T) {
	in := "OPENAI_API_KEY=sk-liveSECRET123456\nAuthorization: Bearer abc.def.secret\nAUTHORIZATION=abc.def.key\npassword: hunter2\nTOKEN='single.secret'\n{\"access_token\":\"json.secret.value\",\"api_key\":\"json-key-value\"}\nok"
	got := RedactDisplayText(in)
	for _, leaked := range []string{"sk-liveSECRET123456", "abc.def.secret", "abc.def.key", "hunter2", "single.secret", "json.secret.value", "json-key-value"} {
		if contains := strings.Contains(got, leaked); contains {
			t.Fatalf("RedactDisplayText leaked %q in %q", leaked, got)
		}
	}
	if !strings.Contains(got, "OPENAI_API_KEY=[redacted]") ||
		!strings.Contains(got, "Authorization: [redacted]") ||
		!strings.Contains(got, "AUTHORIZATION=[redacted]") ||
		!strings.Contains(got, "password: [redacted]") ||
		!strings.Contains(got, "TOKEN='[redacted]'") ||
		!strings.Contains(got, "\"access_token\":\"[redacted]\"") ||
		!strings.Contains(got, "\"api_key\":\"[redacted]\"") ||
		!strings.Contains(got, "ok") {
		t.Fatalf("RedactDisplayText = %q, want key/auth redactions and safe text preserved", got)
	}
}

func TestDisplayStreamSanitizerHandlesSplitStringTerminator(t *testing.T) {
	var s DisplayStreamSanitizer
	if got := s.SanitizeText("\x1b]52;c;SECRET\x1b"); got != "" {
		t.Fatalf("first split chunk = %q, want hidden", got)
	}
	if got := s.SanitizeText("\\ done"); got != " done" {
		t.Fatalf("second split chunk = %q, want visible suffix", got)
	}
}
