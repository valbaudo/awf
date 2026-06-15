package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRunInput(t *testing.T) {
	t.Run("inline only", func(t *testing.T) {
		b, have, err := resolveRunInput(`{"a":1}`, "", nil)
		if err != nil || !have || string(b) != `{"a":1}` {
			t.Fatalf("got (%q, %v, %v), want inline bytes", b, have, err)
		}
	})
	t.Run("neither", func(t *testing.T) {
		b, have, err := resolveRunInput("", "", nil)
		if err != nil || have || b != nil {
			t.Fatalf("got (%q, %v, %v), want (nil,false,nil)", b, have, err)
		}
	})
	t.Run("both flags is a mutual-exclusion error", func(t *testing.T) {
		_, _, err := resolveRunInput(`{"a":1}`, "/tmp/x", nil)
		if err == nil {
			t.Fatal("want mutual-exclusion error, got nil")
		}
	})
	t.Run("file path", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "in.json")
		if err := os.WriteFile(p, []byte(`{"from":"file"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		b, have, err := resolveRunInput("", p, nil)
		if err != nil || !have || string(b) != `{"from":"file"}` {
			t.Fatalf("got (%q, %v, %v), want file bytes", b, have, err)
		}
	})
	t.Run("stdin via dash", func(t *testing.T) {
		b, have, err := resolveRunInput("", "-", strings.NewReader(`{"from":"stdin"}`))
		if err != nil || !have || string(b) != `{"from":"stdin"}` {
			t.Fatalf("got (%q, %v, %v), want stdin bytes", b, have, err)
		}
	})
	t.Run("unreadable file errors", func(t *testing.T) {
		_, _, err := resolveRunInput("", filepath.Join(t.TempDir(), "nope.json"), nil)
		if err == nil {
			t.Fatal("want read error for missing file, got nil")
		}
	})
}
