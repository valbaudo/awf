package goose_test

import (
	"fmt"
	"testing"

	"github.com/valbaudo/awf/agent/goose"
)

// goose stream-json line builders (3 shapes).
func msgLine(role, text string) []byte {
	return []byte(`{"type":"message","message":{"role":"` + role + `","content":[{"type":"text","text":"` + text + `"}]}}`)
}
func completeLine(in, out int) []byte {
	return []byte(fmt.Sprintf(`{"type":"complete","total_tokens":%d,"input_tokens":%d,"output_tokens":%d}`, in+out, in, out))
}

func TestAssistantText_RoleFilter(t *testing.T) {
	asst, _ := goose.ParseStreamEventForTest(msgLine("assistant", "hello"))
	if got := goose.AssistantTextForTest(asst); got != "hello" {
		t.Errorf("assistant text = %q, want %q", got, "hello")
	}
	user, _ := goose.ParseStreamEventForTest(msgLine("user", "ignored"))
	if got := goose.AssistantTextForTest(user); got != "" {
		t.Errorf("user-role text = %q, want empty (filtered)", got)
	}
}

func TestExtractJSONObject_ReassembledDeltas(t *testing.T) {
	// Incremental deltas: "{\"" then "answer\": 4}" reassemble to {"answer": 4}.
	d1, _ := goose.ParseStreamEventForTest(msgLine("assistant", `{\"`))
	d2, _ := goose.ParseStreamEventForTest(msgLine("assistant", `answer\": 4}`))
	final := goose.AssistantTextForTest(d1) + goose.AssistantTextForTest(d2)
	obj, err := goose.ExtractJSONObjectForTest(final)
	if err != nil {
		t.Fatalf("extract: %v (final=%q)", err, final)
	}
	if v, ok := obj["answer"].(float64); !ok || v != 4 {
		t.Errorf("answer = %v, want 4 (final=%q)", obj["answer"], final)
	}
}

func TestExtractJSONObject_DecoyFromNonAssistantExcluded(t *testing.T) {
	// An assistant final object plus a user-role line carrying a decoy {...}. The
	// user line is filtered out, so the assistant's object wins.
	asst, _ := goose.ParseStreamEventForTest(msgLine("assistant", `{\"answer\": 4}`))
	user, _ := goose.ParseStreamEventForTest(msgLine("user", `{\"answer\": 999}`))
	final := goose.AssistantTextForTest(asst) + goose.AssistantTextForTest(user)
	obj, err := goose.ExtractJSONObjectForTest(final)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if v := obj["answer"].(float64); v != 4 {
		t.Errorf("answer = %v, want 4 (decoy must not win)", v)
	}
}
