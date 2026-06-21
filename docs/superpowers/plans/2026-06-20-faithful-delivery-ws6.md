# Faithful Delivery WS-6 (resume incrementality) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use `- [ ]`.

**Goal:** WS-6a — `awf resume --from` can target the *failed (uncommitted) frontier* node (the customer's cheap-resume gap). WS-6b — per-node content-addressing ("edit one step, resume the committed rest"): keep today's whole-workflow digest **fast-path**, and when the definition digest *changed*, engage a **verifying-trace** that reuses committed **deterministic** nodes whose node-key still matches and re-runs the first mismatch + its downstream cone. Addressing-shift and runtime-version drift stay hard errors; agent/react/map/signal/call steps are never content-reused.

**Architecture:** AWF already has a content-addressed blob store + an ordered journal + fold-based resume. WS-6a adds one resolution arm. WS-6b is a *verifying-trace rebuilder* (the "Ninja cell"): `node_key = H(node-subtree-digest ‖ sorted consumed-input blob hashes ‖ runtime pins)`, recorded at commit, folded, and recomputed at resume to gate reuse-vs-rerun — scoped to `*ir.CodeStep`.

**Tech Stack:** Go 1.26, single binary. No new deps. Verified by `make lint test` + the fake-backend conformance suite.

## Global Constraints
- **Go ≥ 1.26.2.** Gate: `make lint test` (gofmt+vet+golangci-lint, `go test -race ./...`, incl. fake conformance).
- **Invariants:** interpreter is the only writer to `state`; commit = content-address-then-pointer-swap; the run id is the only nondeterministic id; **outcomes mechanical-only**; **resume folds the log**. WS-6b PRESERVES "pinning is a hard error on drift" for *addressing-shift* and *runtime-version* drift — it only softens the *whole-definition digest* hard error into a per-node verifying-trace for **deterministic (code) nodes** on an edited definition. The unchanged-digest fast-path is byte-identical to today (zero regression).
- **node_key is informational/derived** — content-addressed (deterministic), excluded from the run id; a node_key mismatch triggers re-run, never silent reuse of stale work.
- **WS-6b gates on a `man §8` revision (Task 4, first WS-6b task)** — the format contract changes; per the doc-hierarchy the man edit ships with the code on this branch. Use the `updating-the-manual` discipline (independent refute + `make man`).
- Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. `docs/` force-added.

---

## Task 1: WS-6a — `--from` targets the failed frontier node

**Files:** `engine/rerun.go` (imports `:3-9`; `lastFailedPath` near `:90`; `ResolveRerunTarget` sig `:98` + new arm after `:107`; doc `:92-97`), `cli/resume.go:232` (call site), `engine/rerun_test.go` + `cli/resume_test.go` (tests).

**Interfaces:** `ResolveRerunTarget(wf *ir.Workflow, rs *RunState, events []state.Event, arg string) (string, error)` (adds `events`); `lastFailedPath(events []state.Event) string`.

- [ ] **Step 1: Failing test** — in `engine/rerun_test.go`, mirror `TestResolveRerunTarget`: pass an `events` slice with a trailing `state.Event{Type: engine.EventNodeFailed, Path: "b"}` and a RunState with only `a` committed; assert `ResolveRerunTarget(wf, rs, events, "b") == "b"` (and the bare-id form). Today fails (compile: arity; then "matches no committed node"). Run RED.
- [ ] **Step 2: Add `lastFailedPath`** (`engine/rerun.go`, after `allCommittedPaths`):
```go
// lastFailedPath returns the Path of the trailing node.failed event, or "".
// node.failed carries the failed frontier node's runtime path on the EVENT
// (e.Path), not its payload, and Fold ignores it — recoverable only from events.
func lastFailedPath(events []state.Event) string {
	last := ""
	for _, e := range events {
		if e.Type == EventNodeFailed && e.Path != "" {
			last = e.Path
		}
	}
	return last
}
```
Add `"github.com/valbaudo/awf/state"` to `engine/rerun.go` imports.
- [ ] **Step 3: Thread `events` + add the arm** — change `ResolveRerunTarget` signature to add `events []state.Event` (after `rs`), and after the root-slot check + before the bare-id `matches` loop, insert:
```go
	// WS-6a: a failed (uncommitted) frontier node has no committed key; accept it
	// if arg names the trailing node.failed path exactly or by its bare trailing id.
	if fp := lastFailedPath(events); fp != "" {
		if arg == fp || arg == fp[strings.LastIndexByte(fp, '.')+1:] {
			return fp, nil
		}
	}
```
Update the doc comment to mention the failed-frontier arm.
- [ ] **Step 4: Call site** — `cli/resume.go:232` → `engine.ResolveRerunTarget(ld.Workflow, rs, events, *from)` (`events` is in scope from `log.Fold()` at `:150`). Fix the `engine/rerun_test.go` existing callers for the new arg.
- [ ] **Step 5: GREEN + a CLI e2e test** — mirror `TestCLIResumeFrom_ReRunsFromStep` but with an `a`-committed + `b`-failed run (use `nodeFailedEvent` `cli/resume_test.go:1363`); assert `--from b` re-runs from `b`. `go test ./engine/ ./cli/ -run "Rerun|ResumeFrom" -v`.
- [ ] **Step 6: `make lint test` + commit** `feat(engine,cli): resume --from can target the failed frontier node (WS-6a)`.

> Note (no change needed): a failed node deep inside a non-`parallel` container is still refused by `rerunSupported` (the existing v1 subset) — the new arm resolves it but `ComputeRerunInvalidation` rejects it, consistent with today. Don't promise more.

---

## Task 2: WS-6b — `nodeSubtreeDigest` (per-node definition hash)

**Files:** `ir/digest.go` (add `nodeSubtreeDigest`; reuse `DigestScheme` `:13`, `writeDigestFrame` `:186`), `ir/digest_test.go`.

- [ ] **Step 1: Failing test** — assert `nodeSubtreeDigest(node)` is stable across map/field reorder (JCS) and changes when the node's `Run`/fields change; two structurally-identical nodes hash equal. Run RED.
- [ ] **Step 2: Implement** (mirror `Workflow.ComputeDigest`'s JCS path, per-node):
```go
// nodeSubtreeDigest is the content hash of a single node's definition subtree
// (RFC-8785/JCS canonical, scheme-prefixed). Reused as one input to the WS-6b
// node_key. Node.MarshalJSON already emits the canonical key-presence shape.
func nodeSubtreeDigest(n Node) (string, error) {
	raw, err := json.Marshal(n)
	if err != nil {
		return "", fmt.Errorf("marshal node: %w", err)
	}
	canon, err := jcs.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("jcs canonicalize node: %w", err)
	}
	h := sha256.New()
	h.Write(canon)
	return DigestScheme + hex.EncodeToString(h.Sum(nil)), nil
}
```
- [ ] **Step 3: GREEN + `make lint test` + commit** `feat(ir): nodeSubtreeDigest for per-node content addressing (WS-6b)`.

---

## Task 3: WS-6b — deterministic-node gate + the `node_key` builder (no wiring yet)

**Files:** `engine/` (new `engine/nodekey.go`), tests. Pure functions, unit-tested in isolation before the Commit/Fold/resume wiring (Tasks 5-6).

**Interfaces:** `isDeterministicNode(n ir.Node) bool` (true only for `*ir.CodeStep`); `computeNodeKey(subtreeDigest string, inputRefs []string, runtimePins []string) string` (sorts inputRefs+pins, frames via the digest-frame encoder, sha256, `DigestScheme`-prefixed).

- [ ] **Step 1: Failing tests** — `isDeterministicNode(*ir.CodeStep{})==true`; `false` for `*ir.AgentStep`, `*ir.React`, `*ir.Map`, `*ir.SignalStep`, `*ir.CallStep`. `computeNodeKey` is order-independent in inputRefs/pins (sorted) and changes when any component changes. Run RED.
- [ ] **Step 2: Implement** — `isDeterministicNode`: `_, ok := n.(*ir.CodeStep); return ok`. `computeNodeKey`: sort `inputRefs` + `runtimePins`, write each through a framed sha256 (mirror `ir.writeDigestFrame` style — or expose/duplicate a tiny framed writer in engine), prefix `ir.DigestScheme`. (Reuse `ir.DigestScheme`; if `writeDigestFrame` is unexported, add a small framed writer in `engine/nodekey.go` — do NOT export ir internals gratuitously.)
- [ ] **Step 3: GREEN + `make lint test` + commit** `feat(engine): deterministic-node gate + node_key builder (WS-6b)`.

---

## Task 4: WS-6b — `man §8` revision (the format gate; ships with the code)

**Files:** `man/awf-workflow.5.md` Pinning paragraph (`:1469-1480`) + the Resume replay clause (`:1453-1457`); then `make man`.

- [ ] **Step 1: Revise the Pinning paragraph** to the verifying-trace model (per the grounded sketch): run-start records the definition digest + runtime pins; each committed node records a **node key** (hash of its definition subtree + sorted input-artifact hashes + runtime pins in scope); on resume an unchanged-digest run replays committed steps as today, and on an **edited** definition a committed **deterministic (code)** node is reused iff its recomputed node key matches, else that node + its downstream cone re-run (instead of the whole-definition hard error); **addressing-shift** edits and **agent-runtime-version** drift remain hard errors; **agent/react/map/signal/call** nodes are never content-reused. Keep the `map` per-element-image clause.
- [ ] **Step 2: Independent refute** (per `updating-the-manual`) — dispatch a fresh refuter to check each revised claim against Tasks 1-3/5-6 code + the still-present addressing-shift/runtime hard errors. Fix flagged items.
- [ ] **Step 3: `make man` (benign groff 128/148 warnings OK) + commit** `git add -f man/awf-workflow.5.md` → `docs(man): per-node verifying-trace resume contract (WS-6b)`.

---

## Task 5: WS-6b — record `node_key` at commit + fold it back

**Files:** `engine/events.go` (`NodeCompletedData` `:393-411`: add `NodeKey string json:"node_key,omitempty"`), `engine/commit.go` (`Commit` `:47-134`: compute the key for deterministic nodes from the node subtree digest + the consumed-input blob refs + runtime pins, set it on the record), `engine/runstate.go` (`NodeResult` `:199-208`: add `NodeKey string`), `engine/fold.go` (`EventNodeCompleted` arm `:187-226`: fold `d.NodeKey`), tests.

> **The load-bearing new plumbing (grounded caveat):** the node's *consumed* input blob refs are NOT assembled at the Commit boundary today. `Commit` must receive (a) the node's `ir.Node` (for `nodeSubtreeDigest`) and (b) the sorted consumed-input blob refs. The implementer must thread these from the dispatch/interpreter call into `Commit` (read the `Commit` call site + how input_files refs are resolved per node in `engine/input_files.go`/`agent_step.go`/`local_dispatcher.go`). For a `*ir.CodeStep` with no declared input_files, the input-ref set is empty (key = H(subtree ‖ ∅ ‖ pins)). Only compute/record the key when `isDeterministicNode(n)`; leave it empty otherwise (omitempty).

- [ ] **Step 1: Failing test** — run a deterministic code step, fold, assert the folded `NodeResult.NodeKey` is non-empty and equals `computeNodeKey(nodeSubtreeDigest(node), inputRefs, pins)`; an agent step records empty NodeKey. Run RED.
- [ ] **Step 2-4:** add the field + compute-at-commit (deterministic-only) + fold-back; thread the node + input refs into `Commit`. GREEN. `make lint test`.
- [ ] **Step 5: Commit** `feat(engine): record per-node node_key at commit + fold (WS-6b)`.

---

## Task 6: WS-6b — resume verifying-trace (fast-path + per-node fallback) + conformance

**Files:** `cli/resume.go` (the digest-drift refusal `:247-256` → keep the fast-path; on `currentDigest != rs.WorkflowDigest` AND `*from==""`, instead of hard-erroring, compute the per-node reuse set and feed the existing invalidation machinery), `engine/` (the verifying-trace: for each committed deterministic node, recompute its expected key from the *current* definition subtree digest + resolved input refs + current pins; mismatch → that node + downstream cone re-run; match → reuse; non-deterministic committed nodes → re-run; the existing path-presence reuse stays for the unchanged-digest fast-path), `conformance/` (new bucket).

> **Keep the fast-path:** when `currentDigest == rs.WorkflowDigest`, behavior is unchanged (path-presence replay — zero per-node work). The verifying-trace only runs on a digest *mismatch*. Addressing-shift (a committed node's top-level slot missing in the current graph — the existing `ComputeRerunInvalidation` check) stays a hard error; runtime-version drift (`CheckRuntimesDrift`) stays a hard error.

- [ ] **Step 1: Failing conformance bucket** (fake backend) — `conformance/resume_pernode.go` + register in `RunSuite`: run a 2-deterministic-step workflow (`a → b`) to completion; EDIT step `b`'s `run:` (changes the digest + b's node_key, not a's); resume; assert **`a` is reused** (its `./a.sh` NOT re-dispatched on the resume fake) and **`b` re-runs** (`./b.sh` dispatched). A second case: edit an UPSTREAM step `a` → both `a` and `b` re-run. A third: an **agent** step is always re-run on any edit. Run RED.
- [ ] **Step 2-4:** implement the verifying-trace + the resume.go fast-path/fallback branch; GREEN; `go test ./conformance/ -run TestConformanceFakeBackend -v` + `make lint test`.
- [ ] **Step 5: Commit** `feat(engine,cli): per-node verifying-trace resume — reuse unchanged deterministic nodes on an edited definition (WS-6b)`.

---

## Self-Review
- WS-6a = T1. WS-6b = T2 (subtree digest) + T3 (gate+key builder) + T4 (man gate) + T5 (record/fold) + T6 (resume trace + conformance).
- The fast-path (unchanged digest) is preserved verbatim — zero regression on normal resume. Per-node trace only on an edited definition. Addressing-shift + runtime drift + agent/non-deterministic nodes stay hard-error/re-run.
- node_key is derived/content-addressed; excluded from the run id; mismatch → re-run (never stale reuse). Threading consumed-input refs into Commit is the one net-new non-trivial piece (T5) — TDD it carefully.

## Known constraints
- WS-6b only content-reuses `*ir.CodeStep` nodes (deterministic). Agent/react/map/signal/call always re-run on an edited definition — by design (non-hermetic). A code step that reads UNDECLARED inputs (undeclared host/container files) can still get a stale reuse (Bazel #16179 class) — documented limitation; declared input_files are the trust boundary, not a sandbox.
