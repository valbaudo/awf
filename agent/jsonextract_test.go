package agent_test

import (
	"testing"

	"github.com/valbaudo/awf/agent"
)

func TestStripThinkTags_ThinkTag(t *testing.T) {
	in := `<think>reasoning goes here</think>{"ok":true}`
	got := agent.StripThinkTags(in)
	want := `{"ok":true}`
	if got != want {
		t.Errorf("StripThinkTags(%q) = %q, want %q", in, got, want)
	}
}

func TestStripThinkTags_ThinkingTag(t *testing.T) {
	in := "...some reasoning...</thinking>{\"verified\":true}"
	got := agent.StripThinkTags(in)
	want := `{"verified":true}`
	if got != want {
		t.Errorf("StripThinkTags(%q) = %q, want %q", in, got, want)
	}
}

func TestStripThinkTags_NoTag(t *testing.T) {
	in := `{"answer":42}`
	got := agent.StripThinkTags(in)
	if got != in {
		t.Errorf("StripThinkTags(%q) = %q, want unchanged", in, got)
	}
}

func TestStripThinkTags_MultipleBlocks_LastTagWins(t *testing.T) {
	in := `<think>block1</think>middle<think>block2</think>{"final":true}`
	got := agent.StripThinkTags(in)
	want := `{"final":true}`
	if got != want {
		t.Errorf("StripThinkTags(%q) = %q, want %q", in, got, want)
	}
}
