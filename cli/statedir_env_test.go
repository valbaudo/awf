package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/valbaudo/awf/cli"
)

// TestAWFStateDirHonoredByLS: with no --state-dir flag, AWF_STATE_DIR selects the
// state directory `ls` reads — proving the env var is honored end-to-end.
func TestAWFStateDirHonoredByLS(t *testing.T) {
	dir := t.TempDir()
	writeMinimalRunLog(t, dir, "env-run")
	t.Setenv("AWF_STATE_DIR", dir)
	var stdout, stderr bytes.Buffer
	rc := cli.Run([]string{"ls", "--output", "json"}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("ls rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "env-run") {
		t.Errorf("AWF_STATE_DIR not honored: ls did not list env-run; got %s", stdout.String())
	}
}

// TestExplicitStateDirOverridesEnv: an explicit --state-dir wins over AWF_STATE_DIR
// (the env points at a different, empty directory).
func TestExplicitStateDirOverridesEnv(t *testing.T) {
	envDir := t.TempDir()
	flagDir := t.TempDir()
	writeMinimalRunLog(t, flagDir, "flag-run") // run exists ONLY under flagDir
	t.Setenv("AWF_STATE_DIR", envDir)          // env points at the empty dir
	var stdout, stderr bytes.Buffer
	rc := cli.Run([]string{"ls", "--state-dir", flagDir, "--output", "json"}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("ls rc = %d, want ExitOK; stderr: %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "flag-run") {
		t.Errorf("explicit --state-dir did not win over AWF_STATE_DIR; got %s", stdout.String())
	}
}
