package agent

import "testing"

func TestIsBareWord(t *testing.T) {
	// every known agent-CLI effort tier — including ones added AFTER an awf
	// release (codex v0.146.0 added max/ultra; the enum this replaces
	// rejected them) — must pass WITHOUT an awf change
	for _, ok := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "somefuturetier"} {
		if !IsBareWord(ok) {
			t.Errorf("IsBareWord(%q) = false, want true", ok)
		}
	}
	// injection surface: the value is shell-quoted into a command line AND
	// interpolated as a bare TOML value (-c key=<value>) — anything but
	// lowercase ASCII letters is rejected
	for _, bad := range []string{"", `high"; x`, "High", "high effort", "high_effort", "high-effort", "1high", "hígh", "a\nb"} {
		if IsBareWord(bad) {
			t.Errorf("IsBareWord(%q) = true, want false", bad)
		}
	}
}
