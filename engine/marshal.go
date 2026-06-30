package engine

import "encoding/json"

// marshalCanonicalJSON is the single serializer for a step's typed output, used
// both at commit (OutputsRef) and by the dispatcher's output_artifact path, so the
// emitted artifact blob is byte-identical to OutputsRef (same content hash, idempotent
// Put). encoding/json sorts object keys lexically → canonical, deterministic bytes.
func marshalCanonicalJSON(v any) ([]byte, error) { return json.Marshal(v) }
