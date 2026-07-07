package engine

import "testing"

// TestEffectiveRecovery: an unset recovery resolves to "continue" for a
// PersistentSession adapter and "restart" for a stateless one; an explicit
// author value is left untouched regardless of adapter.
func TestEffectiveRecovery(t *testing.T) {
	cases := []struct {
		name       string
		authored   string
		persistent bool
		want       string
	}{
		{"unset+persistent→continue", "", true, "continue"},
		{"unset+stateless→restart", "", false, "restart"},
		{"explicit continue kept on stateless", "continue", false, "continue"},
		{"explicit restart kept on persistent", "restart", true, "restart"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveRecovery(tc.authored, tc.persistent); got != tc.want {
				t.Errorf("effectiveRecovery(%q, %v) = %q, want %q", tc.authored, tc.persistent, got, tc.want)
			}
		})
	}
}
