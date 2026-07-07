package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRetryPolicy_ParsesRecovery: recovery parses from a retry block.
func TestRetryPolicy_ParsesRecovery(t *testing.T) {
	var rp RetryPolicy
	if err := json.Unmarshal([]byte(`{"attempts":3,"recovery":"continue"}`), &rp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rp.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", rp.Attempts)
	}
	if rp.Recovery != "continue" {
		t.Fatalf("Recovery = %q, want continue", rp.Recovery)
	}
}

// TestDigest_UnsetRecoveryByteIdentical locks the omitempty invariant for the new
// retry.recovery knob: a retry block that leaves recovery unset must marshal to
// the exact bytes it did before the field existed (no "recovery" key), so
// ComputeDigest — which hashes the whole-workflow JCS — is byte-identical and a
// resume against the same definition never trips the drift hard-error. Fails if
// the field is ever declared without `omitempty`.
func TestDigest_UnsetRecoveryByteIdentical(t *testing.T) {
	wf := &Workflow{
		ID:         "rk-omit",
		Version:    1,
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&AgentStep{
				ID: "gen", Container: "lab", Uses: "test/x",
				Retry: &RetryPolicy{Attempts: 3}, // recovery unset
			},
		},
	}
	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("marshal workflow: %v", err)
	}
	if strings.Contains(string(raw), "recovery") {
		t.Fatalf("unset recovery leaked into digest input JSON (omitempty broken → digest drift): %s", raw)
	}
	// Golden: the digest of this recovery-unset workflow is fixed. If adding the
	// field (or a future change) ever moves these bytes, this fails loudly.
	const want = "awf-d1:sha256:3b2185f66281f80c177a4d6177e4b7e4cb9d78c15ccc3478cedc7ddaff7e46be"
	got, err := wf.ComputeDigest(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("golden digest mismatch:\n got  = %s\n want = %s\n(if this change is intentional, update `want`)", got, want)
	}
}
