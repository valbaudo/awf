package signal

import "testing"

func TestControlDirComposition(t *testing.T) {
	got := ControlDir("/tmp/.awf", "run-x")
	want := "/tmp/.awf/runs/run-x/control"
	if got != want {
		t.Errorf("ControlDir(%q, %q) = %q, want %q", "/tmp/.awf", "run-x", got, want)
	}
}

func TestSignalFileNameRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		seq  int
		file string
	}{
		{"human_review", 1, "signal-human_review-1.json"},
		{"tick", 42, "signal-tick-42.json"},
		{"x", 1, "signal-x-1.json"},
	}
	for _, c := range cases {
		got := signalFileName(c.name, c.seq)
		if got != c.file {
			t.Errorf("signalFileName(%q, %d) = %q, want %q", c.name, c.seq, got, c.file)
		}
		gotName, gotSeq, ok := parseSignalFileName(c.file)
		if !ok {
			t.Errorf("parseSignalFileName(%q): ok=false", c.file)
			continue
		}
		if gotName != c.name || gotSeq != c.seq {
			t.Errorf("parseSignalFileName(%q) = (%q, %d), want (%q, %d)",
				c.file, gotName, gotSeq, c.name, c.seq)
		}
	}
}

func TestParseSignalFileNameRejectsMalformed(t *testing.T) {
	bad := []string{
		"signal.json",
		"signal-.json",
		"signal-name.json",
		"signal-name-x.json",
		"signal-name-0.json", // seq must be ≥ 1
		"signal-name--1.json",
		"pause.json",
		"some-random-file.txt",
	}
	for _, f := range bad {
		if _, _, ok := parseSignalFileName(f); ok {
			t.Errorf("parseSignalFileName(%q) = ok, want !ok", f)
		}
	}
}
