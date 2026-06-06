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

func TestDigestFoldsEnvDeclaration(t *testing.T) {
	// The top-level env: NAMES are part of the definition: changing the declared
	// allowlist changes the digest (so resume hard-errors on a changed declaration),
	// while an absent env: leaves the digest byte-identical (omitempty backwards-compat).
	base, err := sampleWorkflow().ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	withEnv := sampleWorkflow()
	withEnv.Env = []string{"OPENAI_API_KEY"}
	dEnv, err := withEnv.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if base == dEnv {
		t.Fatalf("declaring env: did not change the digest (got %s for both)", base)
	}
	// A nil/empty env: must NOT change the digest vs. pre-env workflows.
	empty := sampleWorkflow()
	empty.Env = nil
	dEmpty, err := empty.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if dEmpty != base {
		t.Fatalf("nil env: changed the digest: %s vs %s", dEmpty, base)
	}
	// A different declared name yields a different digest.
	other := sampleWorkflow()
	other.Env = []string{"LITELLM_API_KEY"}
	dOther, err := other.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if dOther == dEnv {
		t.Fatalf("different env names hashed equal: %s", dEnv)
	}
}

func TestDigestFoldsContinues(t *testing.T) {
	withCont := sampleWorkflow()
	withCont.Graph = append(withCont.Graph,
		&AgentStep{ID: "t1", Uses: "awf/llm"},
		&AgentStep{ID: "t2", Uses: "awf/llm", Continues: "t1"},
	)
	dCont, err := withCont.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	noCont := sampleWorkflow()
	noCont.Graph = append(noCont.Graph,
		&AgentStep{ID: "t1", Uses: "awf/llm"},
		&AgentStep{ID: "t2", Uses: "awf/llm"},
	)
	dNo, err := noCont.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if dCont == dNo {
		t.Fatalf("continues: did not change the digest (got %s for both)", dCont)
	}
}

func TestDigestFoldsWhere(t *testing.T) {
	withWhere := sampleWorkflow()
	withWhere.Graph = append(withWhere.Graph,
		&SignalStep{ID: "s1", Await: "oob-hit", Where: "candidate_id == 1"},
	)
	dWhere, err := withWhere.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	noWhere := sampleWorkflow()
	noWhere.Graph = append(noWhere.Graph,
		&SignalStep{ID: "s1", Await: "oob-hit"},
	)
	dNo, err := noWhere.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if dWhere == dNo {
		t.Fatalf("where: did not change the digest (got %s for both)", dWhere)
	}
}

func TestDigestFoldsAgents(t *testing.T) {
	// A top-level agents: role is part of the definition: declaring it changes
	// the digest (so resume hard-errors on a changed role), while a nil agents:
	// leaves the digest byte-identical (omitempty backwards-compat).
	base, err := sampleWorkflow().ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	withRole := sampleWorkflow()
	withRole.Agents = map[string]AgentRole{
		"auditor": {Uses: "anthropic/claude-code", Model: "opus"},
	}
	dRole, err := withRole.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if base == dRole {
		t.Fatalf("declaring agents: did not change the digest (got %s for both)", base)
	}
	// A nil agents: must NOT change the digest vs. pre-SP2 workflows.
	empty := sampleWorkflow()
	empty.Agents = nil
	dEmpty, err := empty.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if dEmpty != base {
		t.Fatalf("nil agents: changed the digest: %s vs %s", dEmpty, base)
	}
	// A different role uses:/model yields a different digest.
	other := sampleWorkflow()
	other.Agents = map[string]AgentRole{
		"auditor": {Uses: "openai/codex", Model: "gpt-5"},
	}
	dOther, err := other.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if dOther == dRole {
		t.Fatalf("different role uses:/model hashed equal: %s", dRole)
	}
}

func TestDigestFoldsReduce(t *testing.T) {
	// A reduce: clause on a map is part of the definition: declaring it changes
	// the digest (so resume hard-errors on a changed reducer), while a nil
	// Reduce leaves the digest byte-identical (omitempty backwards-compat).
	quorum := Ratio("2")
	withReduce := sampleWorkflow()
	withReduce.Graph = append(withReduce.Graph,
		&Map{Over: "input.items", As: "item", Container: "lab",
			Body:   NodeList{&CodeStep{ID: "b", Run: "x"}},
			Reduce: &Reduce{Quorum: &quorum, Over: "vulnerable"}},
	)
	dReduce, err := withReduce.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	noReduce := sampleWorkflow()
	noReduce.Graph = append(noReduce.Graph,
		&Map{Over: "input.items", As: "item", Container: "lab",
			Body: NodeList{&CodeStep{ID: "b", Run: "x"}}},
	)
	dNo, err := noReduce.ComputeDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if dReduce == dNo {
		t.Fatalf("reduce: did not change the digest (got %s for both)", dReduce)
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
