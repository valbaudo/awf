package live_test

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent/live"
)

func TestRedactKnownSecretShapes(t *testing.T) {
	input := strings.Join([]string{
		"OPENAI_API_KEY=sk-liveSECRET123456",
		"AUTHORIZATION: Bearer abc.def.ghi-secret",
		"proxy-authorization: Basic dXNlcjpwYXNz",
		"password = hunter2",
		"transcript_path=/tmp/provider/raw-session.jsonl",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"FOO_KEY=foo-key",
		"TOKEN='single.secret'",
		`{"access_token":"json.secret.value","api_key":"json-key-value"}`,
	}, "\n")
	got := live.RedactKnownSecretShapes(input)
	for _, leaked := range []string{
		"sk-liveSECRET123456",
		"abc.def.ghi-secret",
		"dXNlcjpwYXNz",
		"hunter2",
		"/tmp/provider/raw-session.jsonl",
		"aws-secret",
		"foo-key",
		"single.secret",
		"json.secret.value",
		"json-key-value",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("RedactKnownSecretShapes leaked %q in %q", leaked, got)
		}
	}
	if strings.Count(got, "[redacted]") < 10 {
		t.Fatalf("RedactKnownSecretShapes = %q, want redaction markers", got)
	}
}
