package cli

import "testing"

func TestParseInputFilesCSV(t *testing.T) {
	eq := func(t *testing.T, got, want map[string]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("got[%q] = %q, want %q (full: %v)", k, got[k], v, got)
			}
		}
	}
	t.Run("repeated form binds each occurrence", func(t *testing.T) {
		got, err := parseInputFilesCSV([]string{"a=x", "b=y"})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, map[string]string{"a": "x", "b": "y"})
	})
	t.Run("legacy single comma-separated value", func(t *testing.T) {
		got, err := parseInputFilesCSV([]string{"a=x,b=y"})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, map[string]string{"a": "x", "b": "y"})
	})
	t.Run("comma in path via repeated form is preserved", func(t *testing.T) {
		got, err := parseInputFilesCSV([]string{"doc=/tmp/a,b.txt", "img=/tmp/c.png"})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, map[string]string{"doc": "/tmp/a,b.txt", "img": "/tmp/c.png"})
	})
	t.Run("empty yields nil map", func(t *testing.T) {
		if got, err := parseInputFilesCSV(nil); err != nil || got != nil {
			t.Fatalf("nil: got (%v, %v), want (nil, nil)", got, err)
		}
		if got, err := parseInputFilesCSV([]string{""}); err != nil || got != nil {
			t.Fatalf("[\"\"]: got (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("malformed entry errors", func(t *testing.T) {
		if _, err := parseInputFilesCSV([]string{"noeq"}); err == nil {
			t.Fatal("want error for entry without '='")
		}
	})
	t.Run("duplicate name errors", func(t *testing.T) {
		if _, err := parseInputFilesCSV([]string{"a=x", "a=y"}); err == nil {
			t.Fatal("want error for duplicate name")
		}
	})
	t.Run("single comma-free entry", func(t *testing.T) {
		got, err := parseInputFilesCSV([]string{"a=x"})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, map[string]string{"a": "x"})
	})
}
