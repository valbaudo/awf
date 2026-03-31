package ir

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
)

// sampleWorkflow includes a nested control node so the digest path exercises recursive marshaling.
func sampleWorkflow() *Workflow {
	return &Workflow{
		ID:         "cve-pipeline",
		Version:    1,
		Containers: map[string]Container{"lab": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&CodeStep{ID: "a", Container: "lab", Run: "x"},
			&Try{Do: NodeList{&Gate{
				Generate: NodeList{&CodeStep{ID: "g", Run: "gen"}},
				Evaluate: NodeList{&CodeStep{ID: "e", Run: "ev"}},
				Until:    "ok", MaxAttempts: 3,
			}}},
		},
	}
}

func TestDigestIsSelfDescribing(t *testing.T) {
	d, err := sampleWorkflow().ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d, digestScheme) {
		t.Fatalf("digest %q lacks scheme prefix", d)
	}
	if len(d) != len(digestScheme)+sha256.Size*2 {
		t.Fatalf("digest %q wrong length", d)
	}
}

func TestDigestExcludesDigestField(t *testing.T) {
	a := sampleWorkflow()
	da, err := a.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	b := sampleWorkflow()
	b.Digest = digestScheme + strings.Repeat("f", sha256.Size*2) // pre-set Digest must not affect the hash
	db, err := b.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatalf("digest changed when Digest field set: %s vs %s", da, db)
	}
}

func TestDigestIndependentOfMapOrder(t *testing.T) {
	a := sampleWorkflow()
	a.Containers = map[string]Container{"z": {Image: "oci://z@sha256:1"}, "a": {Image: "oci://a@sha256:2"}}
	b := sampleWorkflow()
	b.Containers = map[string]Container{"a": {Image: "oci://a@sha256:2"}, "z": {Image: "oci://z@sha256:1"}}
	da, err := a.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatalf("digest depends on map order: %s vs %s", da, db)
	}
}

func TestDigestStableAcrossRoundTrip(t *testing.T) {
	wf := sampleWorkflow()
	d1, err := wf.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	var wf2 Workflow
	if err := json.Unmarshal(raw, &wf2); err != nil {
		t.Fatal(err)
	}
	d2, err := wf2.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("digest changed across round-trip: %s vs %s", d1, d2)
	}
}

func TestDigestFoldsComposeHashes(t *testing.T) {
	wf := sampleWorkflow()
	d1, err := wf.ComputeDigest(map[string][]byte{"lab/compose.yml": []byte("services: {}")})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := wf.ComputeDigest(map[string][]byte{"lab/compose.yml": []byte("services: {x: {}}")})
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatal("digest ignored compose file contents")
	}
}

// Fail-closed golden: no skip branch. Fill `want` from the value this test prints on first failure.
func TestGoldenDigest(t *testing.T) {
	const want = "awf-d1:sha256:073cb3aa4d4a75434f2ef3c247c3efafcb02548d76818870902863bfad31d80e"
	got, err := sampleWorkflow().ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("golden digest mismatch:\n got  = %s\n want = %s\n(if this change is intentional, update `want`)", got, want)
	}
}

func TestSetDigestPopulatesField(t *testing.T) {
	wf := sampleWorkflow()
	d, err := wf.SetDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Digest != d {
		t.Fatalf("Digest field = %q, want %q", wf.Digest, d)
	}
	// Idempotence: SetDigest twice yields the same value (and Digest is excluded from its own hash).
	d2, err := wf.SetDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if d != d2 {
		t.Fatalf("SetDigest changed on re-run: %q vs %q", d, d2)
	}
}

func TestDigestSensitiveToComposePath(t *testing.T) {
	// Same content at different paths must produce different digests — guards the path-framing
	// in ComputeDigest (the path itself is hashed alongside the content's sha256).
	wf := sampleWorkflow()
	content := []byte("services: {}")
	d1, err := wf.ComputeDigest(map[string][]byte{"a/compose.yml": content})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := wf.ComputeDigest(map[string][]byte{"b/compose.yml": content})
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatal("digest ignored compose file path")
	}
}
