# Faithful Delivery WS-1 + WS-2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land WS-1 (P3 skill-asset key qualification in sub-workflows; P12 AWF3002 false positive on gate evaluators) and WS-2 (surface the swallowed `map.item` body-failure cause through journal+fold for both non-prune and prune maps; add the `awfllm` quota/budget→permanent classifier).

**Architecture:** All changes are additive + invariant-preserving in the engine/ir/agent layers. P3 threads `moduleID` into the skill resolver and qualifies the asset-key lookup, mirroring the four sibling resolvers already doing it. P12 marks a gate evaluator's terminal producer `referenced` in the `*Gate` arm of `walkRefs` (the terminal is always a `Code`/`Agent`/`Signal` step — guaranteed by AWF1014). WS-2a adds a forensic, rune-bounded `Cause` to `map.item`/`map.frontier` so a tolerated `item_failed` is no longer causeless — strengthening `crash ≠ verdict`. WS-2b enriches `awfllm`'s permanent predicate with a quota/budget rule (structured `type`/`code` + a LiteLLM-wrapped substring fallback).

**Tech Stack:** Go 1.26, single binary `awf`. Tests use the in-memory `state` + `container.Fake` + `agent/fake` harness. No new dependencies.

## Global Constraints

- **Go ≥ 1.26.2.** Pre-commit gate is **`make lint test`** (gofmt + `go vet` + golangci-lint, then `go test -race ./...` which includes the fake conformance suite). NOT `go test+vet+gofmt`.
- **Invariants (AGENTS.md):** interpreter is the only writer to `state`; outcomes are mechanical-only `{ok, retryable_failure, permanent_failure, rejected}`; `crash ≠ verdict` (WS-2a strengthens it); `with:` stays opaque to the engine; the prune frontier is committed as ONE atomic `map.frontier` event (do not split it; WS-2a rides it additively); the fold never equality-checks event `Data` (a free-text `Cause` is determinism-safe).
- **Commit style:** conventional commits; end each body with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- **Branch:** `worktree-faithful-delivery`. `docs/` is git-ignored → docs commit with `git add -f`; code commits normally.
- **Scope discipline (R8):** do NOT fix the stale `engine/events.go:321` doc comment here (it predates this work — scope creep). Do NOT retroactively add `Outcome` to the prune frontier fold (it would change resume re-run behavior — out of scope; WS-2a threads `Cause` only, which has no resume impact).

---

## Task 1: P3 — qualify the skill asset key in sub-workflows

**Bug:** `buildSkillCorpus` looks up the skill-library directory asset by **bare** `assetID` (`engine/skills.go:80`), but in a called sub-workflow `RunStartedAssets` are keyed `moduleID/assetID` (`engine/assets.go:51-57`). The bare lookup misses → `errArtifactFetch` halt. The skills call at `agent_step.go:88` is the **single** resolver in `runAgentStepWithContext` that omits `ictx.moduleID` (the four siblings at `:172/:190/:210/:222` already pass it).

**Files:**
- Modify: `engine/skills.go` (`selectAgentStepSkills` `:23`/`:32`; `buildSkillCorpus` `:71`/`:80-86`)
- Modify: `engine/agent_step.go:88`
- Create: `engine/skills_internal_test.go` (package **`engine`** — `buildSkillCorpus` is unexported; mirror the `package engine` convention of `engine/local_dispatcher_internal_test.go`)

**Interfaces:**
- Consumes: `QualifiedAssetKey(moduleID, assetID string) string` (`engine/assets.go:13`, returns bare id when `moduleID==""`); `StoreRunStartedAssets(blobs, map[string]ir.LoadedAsset) (map[string]RunStartedAsset, error)` (keys output by the **map key you pass**); `RootModuleID = ""`.
- Produces: `buildSkillCorpus(id string, wf *ir.Workflow, moduleID string, assets map[string]RunStartedAsset, blobs state.Blobs)`; `selectAgentStepSkills(as *ir.AgentStep, path string, wf *ir.Workflow, moduleID string, runstate *RunState, log state.Log, blobs state.Blobs, scope *Scope)`.

> **Red-green ordering (R1):** the test below calls the *new* 5-arg `buildSkillCorpus`, so it does NOT compile against the current 4-arg signature — the compile failure IS the red. The signature change (Step 3) is what turns it green. Steps 1–4 are one red-green unit. The internal test must INLINE its asset map — it cannot reuse `engine/skills_fixture_test.go` helpers (those are `package engine_test`).

- [ ] **Step 1: Write the failing internal unit test** — create `engine/skills_internal_test.go`:

```go
package engine

import (
	"errors"
	"testing"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/skillroute"
	"github.com/valbaudo/awf/state"
)

// TestBuildSkillCorpusQualifiesModuleKey reproduces P3: in a sub-workflow the
// skill-library asset is recorded under QualifiedAssetKey(moduleID, id); the
// bare-id lookup misses. Root (moduleID="") must still resolve via the bare key.
func TestBuildSkillCorpusQualifiesModuleKey(t *testing.T) {
	blobs := state.NewInMemoryBlobs()
	wf := &ir.Workflow{
		Skills: map[string]ir.SkillCorpus{
			"awf": {From: "asset.skill_assets", Layout: skillroute.LayoutSkillDirs, Router: skillroute.RouterName},
		},
	}
	mkAssets := func(key string) map[string]RunStartedAsset {
		a, err := StoreRunStartedAssets(blobs, map[string]ir.LoadedAsset{
			key: {ID: "skill_assets", DeclaredPath: "skills", IsDir: true, Files: []ir.LoadedAssetFile{
				{Path: "billing/SKILL.md", Bytes: []byte("# Billing\nReconcile invoices.\n")},
			}},
		})
		if err != nil {
			t.Fatalf("StoreRunStartedAssets(%q): %v", key, err)
		}
		return a
	}

	// Root: bare key, moduleID "" → resolves.
	if _, err := buildSkillCorpus("awf", wf, "", mkAssets("skill_assets"), blobs); err != nil {
		t.Fatalf("root buildSkillCorpus: unexpected err %v", err)
	}
	// Sub-workflow: assets keyed "child/skill_assets", moduleID "child" → must resolve.
	if _, err := buildSkillCorpus("awf", wf, "child", mkAssets("child/skill_assets"), blobs); err != nil {
		t.Fatalf("sub-workflow buildSkillCorpus: got err %v (errArtifactFetch=%v); want nil", err, errors.Is(err, errArtifactFetch))
	}
}
```

> Confirm field names against `ir.SkillCorpus` (`ir/types.go`), `skillroute.LayoutSkillDirs`/`RouterName`, `ir.LoadedAsset`/`ir.LoadedAssetFile` before running — read the structs if a field doesn't match.

- [ ] **Step 2: Run it red**

Run: `go test ./engine/ -run TestBuildSkillCorpusQualifiesModuleKey -v`
Expected: COMPILE FAILURE (`too many arguments in call to buildSkillCorpus`) — the red driving the signature change.

- [ ] **Step 3: Apply the fix**

`engine/skills.go` — `buildSkillCorpus` signature (`:71`) + lookup (`:80-86`):

```go
func buildSkillCorpus(id string, wf *ir.Workflow, moduleID string, assets map[string]RunStartedAsset, blobs state.Blobs) (*skillroute.Corpus, error) {
```
```go
	key := QualifiedAssetKey(moduleID, assetID)
	asset, ok := assets[key]
	if !ok {
		return nil, fmt.Errorf("%w: skill library %q asset %q was not recorded in run.started", errArtifactFetch, id, key)
	}
	if !asset.IsDir {
		return nil, fmt.Errorf("%w: skill library %q asset %q must be a directory", errArtifactFetch, id, key)
	}
```

`engine/skills.go` — `selectAgentStepSkills` signature (`:23`, add `moduleID` after `wf`) + call (`:32`):

```go
	corpus, err := buildSkillCorpus(as.Skills.From, wf, moduleID, runstate.Assets, blobs)
```

`engine/agent_step.go:88` — pass `ictx.moduleID` after `wf`:

```go
		selectedSkills, skillCorpus, oc, err = selectAgentStepSkills(as, path, wf, ictx.moduleID, runstate, log, blobs, scope)
```

- [ ] **Step 4: Run it green + regression**

Run: `go test ./engine/ -run "TestBuildSkillCorpusQualifiesModuleKey|Skills" -v` → PASS (root unaffected via `QualifiedAssetKey("", id)==id`).

- [ ] **Step 5: Gate + commit**

Run: `make lint test`

```bash
git add engine/skills.go engine/agent_step.go engine/skills_internal_test.go
git commit -m "fix(engine/skills): qualify skill asset key by moduleID in sub-workflows" \
  -m "buildSkillCorpus looked up the skill-library asset by bare assetID; sub-workflow run.started assets are keyed moduleID/assetID, so it missed and halted with errArtifactFetch. Thread moduleID through selectAgentStepSkills->buildSkillCorpus and qualify via QualifiedAssetKey, matching the four sibling resolvers in runAgentStepWithContext. Fixes P3; root (moduleID=\"\") unchanged." \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: P12 — stop AWF3002 firing on an agent-step gate evaluator

**Bug:** AWF3002 (`ir/validate_refs.go:74-79`) flags any `kind=="agent"` producer with an `output_schema` and no inbound `step.<id>` ref. A gate evaluator's terminal is consumed only via `{{ evaluate.<field> }}`, which carries no step id, so `checkRef`'s `evaluate` arm (`:677-683`) never marks it `referenced`. The fix runs in the `*Gate` arm of `walkRefs` (`:323-330`) — the only frame with both the gate node and `referenced` in scope.

**The terminal is ALWAYS a `Code`/`Agent`/`Signal` step — guaranteed by AWF1014** (`ir/validate_structural.go:192-196`: the final evaluate node must declare `output_schema`; `nodeHasOutputSchema` accepts only those three types). So a 3-arm switch is complete; there is **no** nested-control-flow case to handle. Mirror the existing `engine/gate.go:200-218 lastEvaluatorPath`, whose default arm treats a non-step terminal as AWF1014-unreachable.

**Files:**
- Modify: `ir/validate_refs.go` (`*Gate` arm `:323-330`; new helper near `indexModuleProducers` `:143`)
- Modify: `ir/validate_refs_test.go` (new test)

**Interfaces:**
- Produces: `lastEvaluatorProducerID(nodes NodeList) string` — the terminal evaluator producer's step id (`""` only on the AWF1014-unreachable non-step default).

- [ ] **Step 1: Write the failing test** (agent-step evaluator consumed only via `{{ evaluate.verified }}`)

Add to `ir/validate_refs_test.go`:

```go
// TestRefsAgentGateEvaluatorNoAWF3002 covers P12: an agent-step gate evaluator
// whose typed output is consumed only via {{ evaluate.verified }} must NOT trip
// AWF3002. Code-step evaluators never fire it (kind != "agent"), which is why
// this went unnoticed — no existing test uses an *AgentStep evaluator.
func TestRefsAgentGateEvaluatorNoAWF3002(t *testing.T) {
	verifySchema := &JSONSchema{
		"type": "object", "additionalProperties": false,
		"required": []any{"verified"},
		"properties": map[string]any{"verified": map[string]any{"type": "boolean"}},
	}
	ld := makeLD(&Workflow{
		ID: "gate-eval", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Gate{
				Generate:    NodeList{&CodeStep{ID: "gen1", Container: "c", Run: "gen"}},
				Evaluate:    NodeList{&AgentStep{ID: "judge", Container: "c", Uses: "anthropic/claude-code", With: RawConfig{"prompt": "judge it"}, OutputSchema: verifySchema}},
				Until:       "{{ evaluate.verified }}",
				MaxAttempts: 3,
			},
		},
	})
	assertNoCode(t, Validate(ld), "AWF3002")
}
```

- [ ] **Step 2: Run it red**

Run: `go test ./ir/ -run TestRefsAgentGateEvaluatorNoAWF3002 -v`
Expected: FAIL — `assertNoCode` reports an AWF3002 for step `judge`.

- [ ] **Step 3: Add the terminal-producer helper** (mirror `engine/gate.go:200-218`)

In `ir/validate_refs.go`, near `indexModuleProducers`:

```go
// lastEvaluatorProducerID returns the step id of the terminal producer of a
// gate's evaluate list — the node {{ evaluate.<field> }} resolves to. AWF1014
// (validate_structural.go) guarantees that terminal is a Code/Agent/Signal step
// with output_schema, so the default arm is unreachable for a valid gate (it
// mirrors engine/gate.go lastEvaluatorPath). Do NOT add *React/*Call/*Map arms:
// nodeHasOutputSchema rejects them as evaluate terminals.
func lastEvaluatorProducerID(nodes NodeList) string {
	if len(nodes) == 0 {
		return ""
	}
	switch v := nodes[len(nodes)-1].(type) {
	case *CodeStep:
		return v.ID
	case *AgentStep:
		return v.ID
	case *SignalStep:
		return v.ID
	default:
		return "" // AWF1014-unreachable for a valid gate
	}
}
```

> Confirm the type names against `indexModuleProducers`' switch and `engine/gate.go:200-218` before running.

- [ ] **Step 4: Mark the evaluator terminal referenced** — `ir/validate_refs.go:323-330`, append inside `case *Gate:` after the `walkRefs(v.Evaluate, …)` line:

```go
			// The evaluator's terminal typed output is consumed via
			// evaluate.<field> (no step id); mark it referenced so AWF3002 does
			// not flag it. Terminal is always a producer step (AWF1014). P12.
			if id := lastEvaluatorProducerID(v.Evaluate); id != "" {
				referenced[id] = true
			}
```

- [ ] **Step 5: Run it green + regression**

Run: `go test ./ir/ -run "Refs|EvaluateScope|Structural" -v` → PASS (the AWF3002 positive `TestRefsAgentSchemaWithoutRefWarnsAWF3002` still fires; AWF1014/AWF5001 tests stay green).

- [ ] **Step 6: Gate + commit**

Run: `make lint test`

```bash
git add ir/validate_refs.go ir/validate_refs_test.go
git commit -m "fix(ir): suppress AWF3002 on agent-step gate evaluators" \
  -m "A gate evaluator's typed output is consumed only via {{ evaluate.<field> }} (no step id), so the AWF3002 pass never marked it referenced and flagged every agent-step evaluator. Mark the evaluator terminal referenced in the *Gate arm of walkRefs. The terminal is always a Code/Agent/Signal step (AWF1014), mirroring engine/gate.go lastEvaluatorPath. Fixes P12." \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: WS-2a — record the body-failure cause on `map.item` (non-prune)

**Bug:** A tolerated `item_failed` drops `bodyErr`'s text — `commitMapItem` takes no cause param (`engine/map.go:496, 507-521`). This is the gate-in-map swallow (the customer's 16 causeless validation failures — a **non-prune** `min_success` map, covered here). Add a forensic, rune-bounded `Cause` mirroring the existing `Outcome` field (event → fold → record). The cause is richest for agent/staging failures (e.g. `mkdir /skills: permission denied`); a plain code-step exit yields a generic engine message — both are fine (non-empty).

**Files:**
- Modify: `engine/events.go` (`MapItemData` `:117-150`)
- Modify: `engine/map.go` (`dispatchItem` `:495-521`; `commitMapItem` sig `:529`; image_unavailable call `:468` (non-prune); add `boundCause` + `unicode/utf8` import)
- Modify: `engine/runstate.go` (`MapItemRecord` `:95`)
- Modify: `engine/fold.go` (map.item arm `:292-299`)
- Modify: `engine/map_test.go` (new test)

**Interfaces:**
- Produces: `MapItemData.Cause string`; `MapItemRecord.Cause string`; `commitMapItem(log, runstate, mapPath, itemN, status, imageDigest, reason, outcome, cause string)`; `boundCause(err error) string`.

- [ ] **Step 1: Write the failing test** (a two-item map: failing item records non-empty `Cause`, passing item records empty)

Add to `engine/map_test.go`:

```go
// TestMapItemRecordsCause covers WS-2a: a body failure records a non-empty
// bounded Cause on the folded record; a passing item records none.
func TestMapItemRecordsCause(t *testing.T) {
	rig := newMapRig(t, failOn("bad", "echo ok")) // item "bad" exits 1; item "ok" passes
	input := runOverItems("bad", "ok")
	seedRunStartedWithInput(t, rig.lg, rig.blobs, input)
	minSuccess := ir.Ratio("1")
	wf := staticOverWorkflow("x", echoStep("x", &ir.RetryPolicy{Attempts: 1}), 2, &minSuccess)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, input)

	_, _ = runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)

	rs2 := foldFromRig(t, rig)
	items := rs2.LookupMapItems(testMapPath)
	byN := map[int]MapItemRecord{}
	for _, it := range items {
		byN[it.N] = it
	}
	if byN[0].Status != ItemFailed || byN[0].Cause == "" {
		t.Errorf("failed item: Status=%q Cause=%q; want item_failed + non-empty Cause", byN[0].Status, byN[0].Cause)
	}
	if byN[1].Cause != "" {
		t.Errorf("passing item: Cause=%q; want empty", byN[1].Cause)
	}
}
```

> **Implementer note:** `newMapRig` takes a single fake-result helper today (`fail(...)`). If a per-item helper (`failOn(failTok, okRun)`) doesn't exist, add a tiny one next to `fail` in `engine/map_test.go` that programs the fake to exit 1 for the run containing `failTok` and 0 otherwise (read the `fail`/`newMapRig` rig at `map_test.go:36-133` and mirror it). Keep it minimal.

- [ ] **Step 2: Run it red** — `go test ./engine/ -run TestMapItemRecordsCause -v` → FAIL/compile (`Cause` undefined).

- [ ] **Step 3: Add `Cause` to the event + rune-safe bound**

`engine/events.go`, append to `MapItemData` (after `Outcome`):

```go
	// Cause is a bounded, human-readable rendering of the item body's failure
	// (bodyErr) when Status == item_failed. Forensic only — empty for passed/
	// pruned and pre-this-change logs (omitempty). Distinct from Outcome (the
	// mechanical class) and Reason (the infra cause). Not equality-checked by
	// the fold → determinism-safe.
	Cause string `json:"cause,omitempty"`
```

`engine/map.go`, add (mirror `agent/display_helpers.go:72 clip` — byte budget, rune-boundary backup; needs `import "unicode/utf8"`):

```go
// maxMapItemCauseBytes bounds the forensic Cause so a runaway body error can't
// bloat the journal.
const maxMapItemCauseBytes = 1024

// boundCause renders err bounded to a rune boundary (no mid-codepoint cut).
func boundCause(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) <= maxMapItemCauseBytes {
		return s
	}
	end := maxMapItemCauseBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}
```

`engine/map.go` `dispatchItem` (`:495-521`) — add a `cause` local on the failure arm and thread it to `commitMapItem`:

```go
	status := ItemPassed
	itemOutcome := ""
	cause := "" // bounded bodyErr on a body failure (WS-2a)
	var su *SkipUnwind
	if errors.As(bodyErr, &su) {
		if appErr := appendNodeSkipped(ictx.log, itemPath, su.Reason); appErr != nil {
			return "", fmt.Errorf("append node.skipped for item-%d: %w", itemN, appErr)
		}
	} else if bodyErr != nil || bodyOC != OutcomeOK {
		status = ItemFailed
		if bodyOC != OutcomeOK {
			itemOutcome = string(bodyOC)
		}
		cause = boundCause(bodyErr)
	}

	if pr != nil {
		return status, nil // Task 4 threads the cause for prune maps
	}
	return commitMapItem(ictx.log, ictx.runstate, mapPath, itemN, status, imageDigest, "", itemOutcome, cause)
```

`engine/map.go` `commitMapItem` (`:529`) — add `cause` param + field:

```go
func commitMapItem(log state.Log, runstate *RunState, mapPath string, itemN int, status, imageDigest, reason, outcome, cause string) (string, error) {
	data, mErr := json.Marshal(MapItemData{N: itemN, Status: status, ImageDigest: imageDigest, Reason: reason, Outcome: outcome, Cause: cause})
```

`engine/map.go:468` (image_unavailable, non-prune) — add trailing `""`:

```go
	return commitMapItem(ictx.log, ictx.runstate, mapPath, itemN, ItemFailed, "", ReasonImageUnavailable, "", "")
```

- [ ] **Step 4: Fold `Cause` into `MapItemRecord`**

`engine/runstate.go`, add to `MapItemRecord` (`:95`, beside `Outcome`):

```go
	// Cause mirrors MapItemData.Cause. Forensic; read from the FOLDED record.
	Cause string
```

`engine/fold.go` map.item arm (`:292-299`) — add `Cause: d.Cause` to the `RecordMapItem` upsert (copy the existing field set verbatim; add one line):

```go
				Cause:       d.Cause,
```

- [ ] **Step 5: Run it green** — `go test ./engine/ -run "MapItem|FoldMapItem" -v` → PASS.

- [ ] **Step 6: Gate + commit**

Run: `make lint test`

```bash
git add engine/events.go engine/map.go engine/runstate.go engine/fold.go engine/map_test.go
git commit -m "feat(engine/map): record the body-failure cause on map.item (non-prune)" \
  -m "A tolerated item_failed dropped bodyErr's text, leaving only the mechanical Outcome (the gate-in-map swallow). Add a rune-bounded forensic Cause to MapItemData/MapItemRecord, captured in dispatchItem and folded like Outcome. Strengthens crash != verdict; determinism-safe. WS-2a (non-prune; prune in the next task)." \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: WS-2a — thread the cause through the prune frontier (completeness)

**Why separate:** the prune path commits via `commitMapFrontier` (one atomic `map.frontier` event), and `dispatchItem` returns *before* committing for prune maps (`map.go:518-520`), discarding the cause. This is a **completeness** fix (a prune-map body that *fails mechanically* should also record its cause) — NOT the customer's headline case (their `hunt_map` non-survivors are `item_pruned`, which legitimately has no cause). Threading is additive to the existing atomic event (`MapFrontierData.Items` is `[]MapItemData`, which already has the `Cause` field after Task 3) — **atomicity is unaffected** (one event, more fields). We thread **`Cause` only**, NOT `Outcome` (adding `Outcome` to the frontier fold would change resume re-run — out of scope).

**Files:**
- Modify: `engine/map.go` (`dispatchItem` return signature → `(string, string, error)`; the dispatch goroutine `~:225`; the prune final pass `~:277-288`)
- Modify: `engine/fold.go` (map.frontier arm `:313-318`)
- Modify: `engine/map_test.go` (new prune test)

**Interfaces:**
- Produces: `dispatchItem(...) (status string, cause string, err error)`.

- [ ] **Step 1: Write the failing prune test** (a `prune: keep top(N)` map where a body fails mechanically records a non-empty `Cause` on the folded frontier record)

Add to `engine/map_test.go`. Mirror the existing prune test (grep `prune`/`top(` in `map_test.go` for the rig that builds a `prune` map and folds the `map.frontier`); assert the folded `MapItemRecord` for a body-failed item has `Cause != ""`. If no prune rig exists, build one from `staticOverWorkflow` + a `prune` block (read `ir.Map.Prune`/`ir.Prune` in `ir/`). Keep the assertion to: the failed (kept-as-failed) item's `Cause != ""`.

> **Implementer note:** read `engine/prune.go` + the prune branch of `runMap` to confirm which items reach the final pass as `ItemFailed` (vs `ItemPruned`). Assert `Cause` only on an `ItemFailed` frontier item — `ItemPruned` has no cause by design.

- [ ] **Step 2: Run it red** — expect the frontier item's `Cause` empty (or a compile error on the new `dispatchItem` arity).

- [ ] **Step 3: Thread the cause through `dispatchItem` → frontier**

`engine/map.go` — change `dispatchItem` to return `(string, string, error)` (the `cause` local from Task 3 as the middle value). Update every return site:
- early errors: `return "", "", <err>`;
- image-unavailable prune return (`:466`): `return ItemFailed, "", nil`;
- non-prune final: `s, err := commitMapItem(...); return s, cause, err`;
- prune return (`:518-520`): `return status, cause, nil`.

The dispatch goroutine (`~:225-226`) — capture the cause into a parallel `causes` slice (same lock-free index pattern as `statuses`):

```go
	causes := make([]string, len(<over slice>)) // alongside the existing statuses slice
	// inside the goroutine, where statuses[i] = status is set:
	status, cause, dispatchErr := dispatchItem(ctx, n, mapPath, i, pr, ld, ictx)
	statuses[i] = status
	causes[i] = cause
```

The prune final pass (`~:277-288`) — set `Cause` on a failed frontier item:

```go
	data := MapItemData{N: i, Status: final}
	if final == ItemFailed {
		data.Cause = causes[i]
	}
	fresh = append(fresh, data)
```

> Read `map.go:225-293` verbatim first — match the exact slice name (`statuses`) and final-pass construction; do not guess line numbers.

- [ ] **Step 4: Fold the frontier `Cause`**

`engine/fold.go` map.frontier arm (`:313-318`) — add `Cause: it.Cause` to the per-item `RecordMapItem` (do NOT add `Outcome` — out of scope).

- [ ] **Step 5: Run it green** — `go test ./engine/ -run "MapItem|MapFrontier|Prune" -v` → PASS.

- [ ] **Step 6: Gate + commit**

Run: `make lint test`

```bash
git add engine/map.go engine/fold.go engine/map_test.go
git commit -m "feat(engine/map): thread the body cause through the prune frontier" \
  -m "Prune maps commit per-item status via the atomic map.frontier event; dispatchItem returned before committing and dropped the cause. Return the cause from dispatchItem into a lock-free causes slice (mirroring statuses) and set it on a failed frontier item; fold it like Task 3. Atomicity unchanged (one event, additive field). Cause only, not Outcome (no resume-behavior change). WS-2a (prune completeness)." \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: WS-2b — `awfllm` quota/budget → permanent classifier

**Goal:** Today `isPermanentLLMError` flags only `400 + invalid_request_error`; a quota/budget-exhausted response is retryable and burns the retry budget. Classify **permanent** on a quota/budget signal. **Do NOT gate on `status==429`** (OpenAI quota is 429, but LiteLLM budget is observed as 400-or-429; the customer's path is awfllm→LiteLLM). The discriminator is structured `type`/`code` where available (raw OpenAI: `type==code=="insufficient_quota"`), with a **substring fallback** that is genuinely required for the **LiteLLM-wrapped** case (LiteLLM re-wraps OpenAI's `insufficient_quota` into a `RateLimitError(429)` whose body buries the token in a formatted string).

**Scope (R8b):** header capture is **OpenAI-path-only** — only `oe.Response` threads a `*http.Response` into the `apiError` producer; the Ollama/Gemini/Anthropic mappings have a local `resp` but don't pass it (follow-up). Anthropic-native billing (`402 billing_error`, no `code` field) goes through `transport_anthropic.go` — separate, flagged follow-up. The CLI adapters (droid/goose) wrap all errors retryable by design — their budget detection needs their empirical message format (separate follow-up).

**Files:**
- Modify: `agent/awfllm/stream.go` (`apiError` `:26-30`; `isPermanentLLMError` `:32-50`)
- Modify: `agent/awfllm/transport.go` (`classifyOpenAIErr` `:333-343`)
- Modify: `agent/awfllm/stream_test.go` + `agent/awfllm/export_test.go`

**Interfaces:**
- Produces: `apiError{Status int; Type string; Code string; Body string}`; `NewAPIErrorForTest(status int, typ, code, body string)`.

- [ ] **Step 1: Write the failing test** (real wire shapes)

Replace `TestIsPermanentLLMError` in `agent/awfllm/stream_test.go`:

```go
func TestIsPermanentLLMError(t *testing.T) {
	perm := func(status int, typ, code, body string) bool {
		return awfllm.IsPermanentLLMErrorForTest(awfllm.NewAPIErrorForTest(status, typ, code, body))
	}
	// 400 invalid_request (existing) → permanent
	if !perm(400, "invalid_request_error", "", "bad model") {
		t.Error("400 + invalid_request_error should be permanent")
	}
	// raw OpenAI quota: 429, type==code==insufficient_quota → permanent
	if !perm(429, "insufficient_quota", "insufficient_quota", "You exceeded your current quota") {
		t.Error("insufficient_quota must be permanent")
	}
	// LiteLLM-wrapped: type lost, token buried in body → permanent via substring
	if !perm(429, "rate_limit_error", "", "litellm.RateLimitError: OpenAIException - exceeded your current quota, please check your plan and billing") {
		t.Error("LiteLLM-wrapped insufficient_quota (substring) must be permanent")
	}
	// LiteLLM budget exhaustion message → permanent
	if !perm(429, "rate_limit_error", "", "litellm.BudgetExceededError: Budget has been exceeded!") {
		t.Error("LiteLLM budget exceeded must be permanent")
	}
	// plain transient rate limit → retryable
	if perm(429, "rate_limit_exceeded", "rate_limit_exceeded", "Rate limit reached, slow down") {
		t.Error("plain rate_limit must be retryable")
	}
	if awfllm.IsPermanentLLMErrorForTest(errors.New("transport reset")) {
		t.Error("plain transport error must be retryable")
	}
}
```

Update `NewAPIErrorForTest` in `agent/awfllm/export_test.go`:

```go
func NewAPIErrorForTest(status int, typ, code, body string) *apiError {
	return &apiError{Status: status, Type: typ, Code: code, Body: body}
}
```

- [ ] **Step 2: Run it red** — `go test ./agent/awfllm/ -run TestIsPermanentLLMError -v` → FAIL/compile (`Code` undefined; arity changed).

- [ ] **Step 3: Add `Code` to `apiError` + capture it**

`agent/awfllm/stream.go:26-30`:

```go
type apiError struct {
	Status int
	Type   string
	Code   string // provider error.code (OpenAI); "" for synthesized non-OpenAI types
	Body   string
}
```

`agent/awfllm/transport.go:333-343` `classifyOpenAIErr` — capture `oe.Code`:

```go
func classifyOpenAIErr(err error) error {
	var oe *openai.Error
	if errors.As(err, &oe) {
		return &apiError{Status: oe.StatusCode, Type: oe.Type, Code: oe.Code, Body: oe.Error()}
	}
	return err
}
```

- [ ] **Step 4: Add the quota/budget rule**

`agent/awfllm/stream.go:32-50`:

```go
// isPermanentLLMError reports a permanent client-side fault: 400 +
// invalid_request_error, OR a quota/budget-exhausted response (which retry can
// never clear). Quota is matched by structured type/code where available (raw
// OpenAI: insufficient_quota) and by a message-substring fallback that is
// REQUIRED for the LiteLLM-wrapped case (LiteLLM re-wraps OpenAI's
// insufficient_quota into a RateLimitError whose body buries the token). NOT
// gated on status (OpenAI quota is 429; LiteLLM budget is 400-or-429). Plain
// rate-limit (rate_limit_exceeded), 5xx, and transport faults stay retryable.
func isPermanentLLMError(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status == 400 && ae.Type == errTypeInvalidRequest {
		return true
	}
	return isQuotaOrBudget(ae)
}

func isQuotaOrBudget(ae *apiError) bool {
	if ae.Type == "insufficient_quota" || ae.Code == "insufficient_quota" {
		return true
	}
	for _, s := range []string{
		"insufficient_quota",
		"exceeded your current quota",
		"Budget has been exceeded",
		"ExceededBudget",
		"BudgetExceededError",
	} {
		if strings.Contains(ae.Body, s) {
			return true
		}
	}
	return false
}
```

(`strings` is already imported.)

- [ ] **Step 5: Run it green** — `go test ./agent/awfllm/ -run TestIsPermanentLLMError -v` → PASS.

- [ ] **Step 6: Gate + commit**

Run: `make lint test`

```bash
git add agent/awfllm/stream.go agent/awfllm/transport.go agent/awfllm/stream_test.go agent/awfllm/export_test.go
git commit -m "feat(agent/awfllm): classify quota/budget exhaustion as permanent" \
  -m "A quota/budget-exhausted response was retryable and burned the retry budget. Capture oe.Code and classify permanent on insufficient_quota (type/code) plus a message-substring fallback required for the LiteLLM-wrapped case (insufficient_quota / 'Budget has been exceeded'). Not status-gated. Plain rate_limit_exceeded stays retryable. WS-2b. Anthropic-native billing and CLI-adapter (droid/goose) budget detection are flagged follow-ups." \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

- **Spec coverage:** WS-1 = T1 (P3) + T2 (P12). WS-2 = T3 (map.item Cause non-prune) + T4 (prune completeness) + T5 (awfllm quota/budget). 
- **Type consistency:** `Cause` is `string` across event/record/fold; `commitMapItem` gains one trailing `cause`; `dispatchItem` returns `(string, string, error)` after T4; `apiError` gains `Code` and `NewAPIErrorForTest` arity matches.
- **No-placeholder check:** the remaining `> Implementer note` blocks name an exact function to read (`fail`/`newMapRig`, the prune rig, `map.go:225-293`) — not "figure it out." Resolve by reading the named site.

## Cut / deferred (explicit, with reasons)

- **obs map.item projection — CUT (N1).** `obs/control_events.go:19-22` deliberately declines map.item enrichment ("plan-only invention, absent from standard §9, no consumer"). The journal `Cause` (T3/T4) is the durable, queryable surface (`LookupMapItems`/the log). A follow-up should *decide whether obs needs any item attribute at all* — not "finish a stub." (Also: `obs/project.go:126` `StatusMsg = d.Error` is currently unbounded — a latent rune-bounding fix, separate.)
- **`Cause` richness (N2):** rich for agent/staging failures (the customer's `mkdir /skills` case), generic for plain code-step exits. T3's test proves the plumbing, not the richness.
- **Anthropic-native billing** (`402 billing_error`, no `code`) and **CLI-adapter (droid/goose) budget detection** — separate follow-ups (different transport / need empirical message format).
- **Full sub-workflow `imports:`+`call:` conformance bucket for P3** — the engine-level internal test is the tighter red/green; the e2e bucket is a recommended follow-up.
