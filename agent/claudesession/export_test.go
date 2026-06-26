package claudesession

// Exported test helpers for the external test package.

// SessionUUIDForTest exposes sessionUUID to external tests.
var SessionUUIDForTest = sessionUUID

// EncodeProjectDirForTest exposes encodeProjectDir to external tests.
var EncodeProjectDirForTest = encodeProjectDir

// AssembleSessionCommandForTest exposes assembleSessionCommand to external tests.
var AssembleSessionCommandForTest = assembleSessionCommand

// AdapterEnvForTest returns a snapshot of the adapter's internal env map for
// mutation-detection tests. The returned map is a copy so callers cannot
// accidentally mutate the adapter's state through it.
func AdapterEnvForTest(a *Adapter) map[string]string {
	out := make(map[string]string, len(a.env))
	for k, v := range a.env {
		out[k] = v
	}
	return out
}
