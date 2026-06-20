package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"slices"

	"github.com/valbaudo/awf/ir"
)

// isDeterministicNode reports whether n is a deterministic node kind whose
// output is fully determined by its definition subtree and its input values.
// Only CodeStep qualifies: shell commands are pure functions of their inputs
// (same script, same input artifacts → same output), so the engine can
// short-circuit re-execution when all inputs match.
//
// All other node kinds are NOT deterministic:
//   - AgentStep / React: LLM sampling is stochastic.
//   - Map: fan-out over a dynamic collection.
//   - SignalStep / CallStep: depend on external events or sub-workflow side-effects.
//   - Control nodes (If/Loop/Try/Parallel/Gate/Skip/Compose): structural wrappers,
//     not independently executable steps.
func isDeterministicNode(n ir.Node) bool {
	_, ok := n.(*ir.CodeStep)
	return ok
}

// computeNodeKey derives a stable, collision-resistant cache key for a
// deterministic node from three inputs:
//
//  1. subtreeDigest — the scheme-prefixed sha256 of the node's JCS-canonical
//     definition (produced by ir.nodeSubtreeDigest, supplied by Task 5).
//  2. inputRefs — ordered list of artifact refs that feed this node
//     (e.g. "step.fetch.files.out"). Sorted before hashing so insertion order
//     in the workflow graph does not affect the key.
//  3. runtimePins — additional runtime-determined values that affect the node
//     (e.g. workflow input hash, image digest). Sorted before hashing.
//
// Framing: each string is written as its uint64 little-endian byte-length
// followed by the raw bytes. Each list is prefixed with its uint64 count.
// This makes the encoding injective: ["a","bc"] ≠ ["ab","c"] (element
// boundaries are explicit) and [refs…] + [pins…] ≠ [refs+pins…] + [] (the
// list count separates the two sections). The hash stream is:
//
//	<subtreeDigest bytes (framed)>
//	<uint64 len(inputRefs)>  <uint64 len(ref₀)><ref₀> …
//	<uint64 len(runtimePins)> <uint64 len(pin₀)><pin₀> …
//
// Returns ir.DigestScheme + hex(sha256(framed stream)).
func computeNodeKey(subtreeDigest string, inputRefs []string, runtimePins []string) string {
	// Clone and sort to avoid mutating caller slices.
	refs := append([]string(nil), inputRefs...)
	pins := append([]string(nil), runtimePins...)
	slices.Sort(refs)
	slices.Sort(pins)

	h := sha256.New()
	frameString(h, subtreeDigest)
	frameList(h, refs)
	frameList(h, pins)
	return ir.DigestScheme + hex.EncodeToString(h.Sum(nil))
}

// frameString writes a length-prefixed string to w.
func frameString(w interface{ Write([]byte) (int, error) }, s string) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(len(s)))
	_, _ = w.Write(buf[:])
	_, _ = w.Write([]byte(s))
}

// frameList writes the count of the list, then each element as a framed string.
func frameList(w interface{ Write([]byte) (int, error) }, ss []string) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(len(ss)))
	_, _ = w.Write(buf[:])
	for _, s := range ss {
		frameString(w, s)
	}
}
