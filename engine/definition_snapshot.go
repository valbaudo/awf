package engine

import (
	"encoding/json"
	"fmt"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

// StoreRunStartedDefinitionSnapshot serializes the full loaded definition (canonical IR) and
// stores it as a single content-addressed blob, returning the ref. The snapshot is the run's own
// copy of its definition, recorded once at run start so a read-only viewer can render the run
// faithfully against the structure it actually executed against — even after the on-disk file is
// edited (e.g. `awf ui` overlaying a past run).
//
// It is NEVER consulted for resume or pinning: §8 drift is always decided against the live file
// (cli/resume.go, cli/outputs.go). This is a view-only artifact. CAS dedup means repeated runs of
// an unedited definition share one blob. A nil definition yields an empty ref (no blob written).
func StoreRunStartedDefinitionSnapshot(blobs state.Blobs, ld *ir.LoadedDefinition) (string, error) {
	if ld == nil {
		return "", nil
	}
	raw, err := json.Marshal(ld)
	if err != nil {
		return "", fmt.Errorf("marshal definition snapshot: %w", err)
	}
	ref, err := blobs.Put(raw)
	if err != nil {
		return "", fmt.Errorf("put definition snapshot: %w", err)
	}
	return ref, nil
}

// LoadRunStartedDefinitionSnapshot is the inverse of StoreRunStartedDefinitionSnapshot: it fetches
// the snapshot blob and reconstructs the loaded definition. ir.LoadedDefinition round-trips through
// JSON byte-identically (ir.NodeList's marshalers and ir.unmarshalNode are exact inverses), so
// graph projection over the reconstructed definition matches the original.
func LoadRunStartedDefinitionSnapshot(blobs state.Blobs, ref string) (*ir.LoadedDefinition, error) {
	raw, err := blobs.Get(ref)
	if err != nil {
		return nil, fmt.Errorf("get definition snapshot %q: %w", ref, err)
	}
	var ld ir.LoadedDefinition
	if err := json.Unmarshal(raw, &ld); err != nil {
		return nil, fmt.Errorf("unmarshal definition snapshot %q: %w", ref, err)
	}
	return &ld, nil
}
