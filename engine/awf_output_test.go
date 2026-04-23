package engine

import "testing"

func TestAWFOutputTempPathGolden(t *testing.T) {
	cases := []struct {
		nodePath, want string
	}{
		{"triage", "/tmp/awf/triage.json"},
		{"echo_step", "/tmp/awf/echo_step.json"},
		// Control-flow paths sanitize [/].
		{"try[0].do.exploit", "/tmp/awf/try_0__do_exploit.json"},
		{"loop[0].body.iter-3.scan", "/tmp/awf/loop_0__body_iter-3_scan.json"},
		{"parallel[2].run", "/tmp/awf/parallel_2__run.json"},
		{"map[0].item-3.scan", "/tmp/awf/map_0__item-3_scan.json"},
		{"gate[0].attempt-2.evaluate.run_oracle", "/tmp/awf/gate_0__attempt-2_evaluate_run_oracle.json"},
		// Existing safe chars (alphanumerics, _, -) pass through.
		{"a-b_c_123", "/tmp/awf/a-b_c_123.json"},
	}
	for _, c := range cases {
		got := awfOutputTempPath(c.nodePath)
		if got != c.want {
			t.Errorf("awfOutputTempPath(%q) = %q, want %q", c.nodePath, got, c.want)
		}
	}
}
