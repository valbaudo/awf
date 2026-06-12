package cli

import "testing"

func TestFormatUSD(t *testing.T) {
	cases := map[float64]string{
		0:        "$0.0000",
		0.0123:   "$0.0123",
		1.5:      "$1.5000",
		12.34567: "$12.3457",
	}
	for in, want := range cases {
		if got := formatUSD(in); got != want {
			t.Errorf("formatUSD(%v) = %q, want %q", in, got, want)
		}
	}
}
