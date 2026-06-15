package cli

import "testing"

// TestDefaultStateDir locks the --state-dir default seed: AWF_STATE_DIR when set
// and non-empty, otherwise ".awf". (An explicit --state-dir overriding the env is
// covered end-to-end in the cli_test behavioral tests, since pflag uses the
// default only when the flag is absent.)
func TestDefaultStateDir(t *testing.T) {
	t.Run("env set wins over baseline", func(t *testing.T) {
		t.Setenv("AWF_STATE_DIR", "/tmp/custom-state")
		if got := defaultStateDir(); got != "/tmp/custom-state" {
			t.Errorf("defaultStateDir() = %q, want /tmp/custom-state", got)
		}
	})
	t.Run("empty env falls back to .awf", func(t *testing.T) {
		t.Setenv("AWF_STATE_DIR", "")
		if got := defaultStateDir(); got != ".awf" {
			t.Errorf("defaultStateDir() = %q, want .awf", got)
		}
	})
}
