package conformance

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/engine"
)

func testArtifactContracts(t *testing.T, factory BackendFactory) {
	t.Helper()
	if _, ok := factory().(*container.Fake); !ok {
		t.Skip("artifact contract bucket programs fake output_files; fake-only")
	}
	t.Run("jsonl_schema_ref_rejects_invalid_capture", func(t *testing.T) {
		testArtifactContractJSONLSchemaRefRejectsInvalidCapture(t, factory)
	})
}

func testArtifactContractJSONLSchemaRefRejectsInvalidCapture(t *testing.T, factory BackendFactory) {
	t.Helper()
	h := newHarness(t, func() container.Backend {
		fake := factory().(*container.Fake)
		fake.ProgramExecWithFiles("./produce.sh", container.ExecResult{ExitCode: 0}, nil,
			map[string][]byte{"/out/rows.jsonl": []byte("{\"id\":\"bad\"}\n")})
		return fake
	}, artifactContractWorkflow)

	writeAssetFile(t, filepath.Join(h.baseDir, "schemas", "row.schema.json"),
		[]byte(`{"type":"object","required":["id"],"properties":{"id":{"type":"integer"}}}`))

	oc, err := h.runWorkflow(t)
	if err == nil || !strings.Contains(err.Error(), "artifact contract") || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("run err = %v, want artifact contract line failure", err)
	}
	if oc != engine.OutcomeRetryableFailure {
		t.Fatalf("outcome = %q, want retryable_failure", oc)
	}
	rs, ferr := engine.Fold(mustFoldEvents(t, h), h.blobs)
	if ferr != nil {
		t.Fatalf("Fold: %v", ferr)
	}
	if _, ok := rs.Completed["produce"]; ok {
		t.Fatal("invalid artifact contract committed node.completed")
	}
}

var artifactContractWorkflow = fmt.Sprintf(`workflow: conformance-artifact-contracts
version: 1
assets:
  row_schema: schemas/row.schema.json
containers:
  lab:
    image: %s
graph:
  - id: produce
    container: lab
    run: "./produce.sh"
    retry: { attempts: 1 }
    output_files:
      rows:
        path: /out/rows.jsonl
        format: jsonl
        schema_ref: asset.row_schema
`, fakeImageDigest)
