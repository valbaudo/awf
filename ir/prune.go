package ir

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Prune is the optional frontier clause on a map (spec §3.2b). It reads a typed
// numeric `score` field per item as items commit and cancels in-flight/queued
// losers. Exactly one of Keep / StopWhen is set (AWF1037).
//
// The whole struct folds into the workflow definition digest via the Map field's
// omitempty pointer — a nil Prune keeps pre-SP5 digests byte-identical; a non-nil
// Prune is pinned, so resume against a changed policy is a hard drift error (the
// existing pinning guard).
type Prune struct {
	// Score is the name of the NUMERIC field the body's last step declares in
	// its output_schema. The engine reads it from the committed item's typed
	// outputs (never parses text). Required (AWF1037 / AWF5008).
	Score string `json:"score"`
	// Keep is the "keep the top k scorers" policy. Wire form: keep: <k> (a
	// plain positive integer; F21 dropped the earlier top(<k>) wrapper — it
	// was the format's only function-call-shaped literal and top-k was
	// always the only mode, so the wrapper carried zero information).
	// Mutually exclusive with StopWhen (AWF1037).
	Keep *PruneKeep `json:"keep,omitempty"`
	// StopWhen is a bounded boolean expression over `best.score` (the running
	// best). Once true, all still-running/queued items are pruned. Mutually
	// exclusive with Keep (AWF1037). Evaluated via template.EvalBoolString.
	StopWhen string `json:"stop_when,omitempty"`
}

// PruneKeep is the "keep the top k scorers" policy. Custom (un)marshal so the
// wire form is a plain positive JSON integer while the in-memory form is a
// typed K — mirroring how Skip/Parallel carry a non-object scalar through
// custom (un)marshalers. NOTE: K has no json tag (the custom marshaler owns
// the wire form), so PruneKeep is deliberately NOT in ir/tags_test.go's
// irTypes() list (Task 2 Step 5).
//
// F21: the wire form used to be the function-call-shaped string "top(<k>)" —
// the format's only literal of that shape, and pure noise since top-k was
// always the only mode. It is now a plain positive integer; the removed
// string form is a hard decode-time rejection, not a silent alias.
type PruneKeep struct {
	K int
}

// UnmarshalJSON accepts a plain positive JSON integer. The removed top(<k>)
// string form gets a migration-specific message; any other non-integer or
// non-positive value gets AWF1037's "keep must be a positive integer".
func (p *PruneKeep) UnmarshalJSON(b []byte) error {
	var k int
	if err := json.Unmarshal(b, &k); err != nil {
		var s string
		if json.Unmarshal(b, &s) == nil && strings.HasPrefix(strings.TrimSpace(s), "top(") {
			return fmt.Errorf("keep: use a plain positive integer (`top(<k>)` was removed)")
		}
		return fmt.Errorf("prune.keep: keep must be a positive integer")
	}
	if k <= 0 {
		return fmt.Errorf("prune.keep %d: keep must be a positive integer", k)
	}
	p.K = k
	return nil
}

// MarshalJSON re-emits the canonical plain-integer wire form (digest-stable).
func (p PruneKeep) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.K)
}

// LastStepID returns the id of the body's LAST node when that node is a
// code/agent step, plus a present flag. The prune controller reads the score
// from the committed step output at ItemPath(mapPath, N) + "." + id, so the
// engine needs the id of the score-producing step; the validator already
// guarantees (AWF5008) the last node is a step with a numeric score field.
// Returns ("", false) for an empty body or a control-flow terminal node —
// mirrors lastStepSchema's walk so engine + validator agree on which node
// produces the score.
func LastStepID(body NodeList) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	switch s := body[len(body)-1].(type) {
	case *CodeStep:
		return s.ID, true
	case *AgentStep:
		return s.ID, true
	default:
		return "", false
	}
}
