package docker

import "testing"

func TestContainerNameGolden(t *testing.T) {
	cases := []struct {
		runID, declared, want string
	}{
		{"run-abc", "lab", "awf-run-abc-lab"},
		{"01HXYZ", "workspace", "awf-01HXYZ-workspace"},
		// run.id is a ULID-shaped string in production; hyphens are fine —
		// we only format, never parse.
		{"run-1-2-3", "lab", "awf-run-1-2-3-lab"},
	}
	for _, c := range cases {
		got := containerName(c.runID, c.declared)
		if got != c.want {
			t.Errorf("containerName(%q, %q) = %q, want %q", c.runID, c.declared, got, c.want)
		}
	}
}

func TestContainerPrefixGolden(t *testing.T) {
	// The prefix is used by the orphan sweep (cleanupOrphans in
	// backend_integ_test.go) to filter container names by run-id scope.
	if got := containerPrefix("run-abc"); got != "awf-run-abc-" {
		t.Errorf("containerPrefix = %q, want \"awf-run-abc-\"", got)
	}
}
