package ir

import (
	"encoding/json"
	"fmt"
	"strconv"
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
	// Keep is the "keep the top k scorers" policy. Wire form: keep: top(<k>).
	// Mutually exclusive with StopWhen (AWF1037).
	Keep *PruneKeep `json:"keep,omitempty"`
	// StopWhen is a bounded boolean expression over `best.score` (the running
	// best). Once true, all still-running/queued items are pruned. Mutually
	// exclusive with Keep (AWF1037). Evaluated via template.EvalBoolString.
	StopWhen string `json:"stop_when,omitempty"`
}

// PruneKeep is the parsed top(k) policy. Custom (un)marshal so the wire form is
// the string "top(<k>)" while the in-memory form is a typed K — mirroring how
// Skip/Parallel carry a non-object scalar through custom (un)marshalers. NOTE:
// K has no json tag (the custom marshaler owns the wire form), so PruneKeep is
// deliberately NOT in ir/tags_test.go's irTypes() list (Task 2 Step 5).
type PruneKeep struct {
	K int
}

// UnmarshalJSON parses the wire string "top(<k>)" into K (AWF1037 on bad shape).
func (p *PruneKeep) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("prune.keep must be the string top(<k>): %w", err)
	}
	pk, err := ParsePruneKeep(s)
	if err != nil {
		return err
	}
	*p = pk
	return nil
}

// MarshalJSON re-emits the canonical "top(<k>)" wire form (digest-stable).
func (p PruneKeep) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprintf("top(%d)", p.K))
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

// ParsePruneKeep parses "top(<k>)" into a PruneKeep with a positive K. Trims
// surrounding and inner whitespace. Any other shape (wrong head, non-int,
// k <= 0, unbalanced parens) is an error — the validator surfaces it as AWF1037.
func ParsePruneKeep(raw string) (PruneKeep, error) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "top(") || !strings.HasSuffix(s, ")") {
		return PruneKeep{}, fmt.Errorf("prune.keep %q: expected top(<k>)", raw)
	}
	inner := strings.TrimSpace(s[len("top(") : len(s)-1])
	k, err := strconv.Atoi(inner)
	if err != nil {
		return PruneKeep{}, fmt.Errorf("prune.keep %q: top(<k>) needs an integer k: %w", raw, err)
	}
	if k <= 0 {
		return PruneKeep{}, fmt.Errorf("prune.keep %q: k must be a positive integer", raw)
	}
	return PruneKeep{K: k}, nil
}
