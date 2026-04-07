package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// -update regenerates the golden files. The Phase-1 plan locks the golden text/JSON shape;
// running `go test ./cli/ -update` is how the maintainer regenerates them after an
// intentional format change (e.g. tweaking the "1 error" summary line).
var updateGolden = flag.Bool("update", false, "regenerate golden files in cli/testdata/golden/")

// goldenScenario describes one CLI invocation declaratively. Args are built programmatically
// from `fixture` + `asJSON` so a future flag-syntax variant (`--format=json`, etc.) can't
// desync the .txt/.json golden lookup from what the test actually ran. The scenario's
// expected exit code is part of the case so the test catches drift in the contract.
type goldenScenario struct {
	name     string // golden-file basename (without extension)
	fixture  string // workflow path (resolved by cli.Run as the validate subcommand's positional arg)
	asJSON   bool   // true → adds --format json AND selects .json golden; false → text mode + .txt golden
	wantCode int    // expected cli.Run exit code
}

// scenarios is the closed list of golden cases. Keep small and representative — the inline
// unit tests in validate_test.go cover every branch; this test just locks the wire shape
// against real fixtures. Adding every AWF*-coded invalid fixture would be redundant with
// ir/validate_golden_test.go from slice 1.4 (which already golden-tests the diagnostics).
var scenarios = []goldenScenario{
	{name: "valid-cve-pipeline-text", fixture: "../loader/testdata/valid/cve-pipeline.yaml", asJSON: false, wantCode: ExitOK},
	{name: "valid-cve-pipeline-json", fixture: "../loader/testdata/valid/cve-pipeline.yaml", asJSON: true, wantCode: ExitOK},
	{name: "invalid-duplicate-step-id-text", fixture: "../ir/testdata/invalid/AWF1004-duplicate-step-id.yaml", asJSON: false, wantCode: ExitInvalid},
	{name: "invalid-duplicate-step-id-json", fixture: "../ir/testdata/invalid/AWF1004-duplicate-step-id.yaml", asJSON: true, wantCode: ExitInvalid},
	{name: "invalid-image-not-digest-text", fixture: "../ir/testdata/invalid/AWF1007-image-not-digest.yaml", asJSON: false, wantCode: ExitInvalid},
}

func TestValidateGolden(t *testing.T) {
	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			// Build args declaratively from the scenario — no fragile string-search for
			// "json" in the args slice, which would false-positive on `--format=json` or a
			// path containing the substring.
			args := []string{"validate"}
			ext := ".txt"
			if sc.asJSON {
				args = append(args, "--format", "json")
				ext = ".json"
			}
			args = append(args, sc.fixture)

			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr)
			if code != sc.wantCode {
				t.Errorf("exit = %d, want %d (stderr=%q)", code, sc.wantCode, stderr.String())
			}

			goldenPath := filepath.Join("testdata", "golden", sc.name+ext)
			got := stdout.Bytes()
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v (run `go test ./cli/ -update` to create)", goldenPath, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("output diverged from %s\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
			}
		})
	}
}
