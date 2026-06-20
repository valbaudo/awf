# Faithful Delivery WS-1 + WS-2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land WS-1 (two confirmed defects: P3 skill-asset key qualification in sub-workflows; P12 AWF3002 false positive on gate evaluators) and WS-2 (richer typed causes: surface the swallowed `map.item` body-failure cause through journal+fold+obs; add the `awfllm` layered 429 classifier).

**Architecture:** All changes are in the engine/ir/agent layers and are additive + invariant-preserving. P3 threads `moduleID` into the skill resolver and qualifies the asset-key lookup (mirroring the existing `input_files` resolver). P12 marks a gate evaluator's terminal producer `referenced` in the `*Gate` arm of `walkRefs`. WS-2a adds a forensic `Cause` field to the `map.item` event + folded record (mirroring the existing `Outcome` field) so a tolerated `item_failed` is no longer causeless — strengthening `crash ≠ verdict`. WS-2b enriches `awfllm`'s permanent-vs-retryable predicate with a layered 429 rule using newly-captured response headers.

**Tech Stack:** Go 1.26, single binary `awf`. Tests use the in-memory `state` + `container.Fake` + `agent/fake` harness. No new dependencies.

## Global Constraints

- **Go ≥ 1.26.2** (CVE-2026-32282 floor; matches repo).
- **Pre-commit gate is `make lint test`** — NOT `go test + go vet + gofmt`. `make lint` runs `gofmt -l`, `go vet ./...`, and `golangci-lint` (errcheck/staticcheck). CI runs `make lint test build`.
- **`make test` = `go test -race ./...`** and includes the fake-backed conformance suite. There is NO separate `conformance` target.
- **Invariants that must not break** (AGENTS.md): the interpreter is the only writer to `state`; outcomes are mechanical-only `{ok, retryable_failure, permanent_failure, rejected}` (do not add a class); `crash ≠ verdict` (WS-2a strengthens it); `with:` stays opaque to the engine; resume folds the log and the fold never hashes/equality-checks event `Data` (so a free-text `Cause` is determinism-safe).
- **Commit style:** conventional commits; end every commit message body with a trailing line:
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`
- **Branch:** work is on `worktree-faithful-delivery`. Design spec: `docs/superpowers/specs/2026-06-20-awf-faithful-delivery-design.md`.
- **`docs/` is git-ignored** — design/plan files are force-added (`git add -f`); code files commit normally.

---

## Task 1: P3 — qualify the skill asset key in sub-workflows

**Bug:** `buildSkillCorpus` looks up the skill-library directory asset by **bare** `assetID` (`engine/skills.go:80`), but in a called sub-workflow `RunStartedAssets` are keyed `moduleID + "/" + assetID` (`engine/assets.go:51-57`). The bare lookup misses → `errArtifactFetch` halt. Every other asset resolver already qualifies via `QualifiedAssetKey(moduleID, id)` (`engine/input_files.go:200-205`).

**Files:**
- Modify: `engine/skills.go` (signatures + lookup at `:23`, `:32`, `:71`, `:80-86`)
- Modify: `engine/agent_step.go:88` (the one call site)
- Test: `engine/skills_run_test.go` (new regression test, package `engine_test`)
- Test support already present: `engine/skills_fixture_test.go` (`skillWorkflowFixture`, `storeTestSkillAssets`, `runAgentSkillsStagingFixture`)

**Interfaces:**
- Consumes: `engine.QualifiedAssetKey(moduleID, assetID string) string` (`engine/assets.go:13`), `RootModuleID = ""`.
- Produces: `buildSkillCorpus(id string, wf *ir.Workflow, moduleID string, assets map[string]RunStartedAsset, blobs state.Blobs)` and `selectAgentStepSkills(as *ir.AgentStep, path, moduleID string, wf *ir.Workflow, runstate *RunState, log state.Log, blobs state.Blobs, scope *Scope)`.

- [ ] **Step 1: Write the failing regression test** (a skills run under a non-root `moduleID` whose assets are qualified-keyed)

Add to `engine/skills_run_test.go`:

```go
// TestRunAgentStep_SkillsQualifiedAssetKeyInSubWorkflow reproduces P3: when the
// skill-library asset is recorded under a module-qualified key (as it is for a
// called sub-workflow), buildSkillCorpus must look it up by QualifiedAssetKey,
// not the bare assetID. Red before the fix (errArtifactFetch), green after.
func TestRunAgentStep_SkillsQualifiedAssetKeyInSubWorkflow(t *testing.T) {
	const moduleID = "child"
	def, bareAssets, blobs := skillWorkflowFixture(t, ir.Template("kubernetes pod network outage"), nil)

	// Re-key the stored assets under the module-qualified key, mimicking
	// StoreRunStartedAssetsForLoadedDefinition for a non-root module.
	qualAssets := map[string]engine.RunStartedAsset{}
	for id, a := range bareAssets {
		qualAssets[engine.QualifiedAssetKey(moduleID, id)] = a
	}

	rs := engine.NewRunState("r1", "d", nil)
	rs.Assets = qualAssets

	// Drive the agent step with the child moduleID in scope.
	runAgentSkillsStagingFixtureModule(t, def, rs, qualAssets, blobs, moduleID)
}
```

Add the module-aware harness variant to `engine/skills_fixture_test.go` (mirrors `runAgentSkillsStagingFixture` but sets the interpreter `moduleID`). If the existing `engine.Run`/dispatch entry does not expose a `moduleID` knob for a single-step run, drive the step through the same module path the call step uses — read `engine/call_step.go:98` (`childCtx.moduleID = child.ID`) and `engine/interpreter_context.go:16` to wire it. Minimal form, reusing `runAgentWithStateAndContainer` but asserting success:

```go
func runAgentSkillsStagingFixtureModule(t *testing.T, def *ir.LoadedDefinition, rs *engine.RunState, assets map[string]engine.RunStartedAsset, blobs state.Blobs, moduleID string) {
	t.Helper()
	// The fixture is a single root workflow; to exercise the qualified-key path
	// we run it as the child module `moduleID`. Use the call-step module entry
	// (engine wraps the def as an imported module keyed by moduleID).
	runAgentWithModuleID(t, def, rs, assets, blobs, moduleID)
}
```

> **Implementer note:** `runAgentWithModuleID` does not exist yet. The cleanest reproduction is a true `imports:`+`call:` workflow where the child declares the skill library. If wiring a single-step run under a synthetic `moduleID` is not reachable through the public `engine.Run` surface, instead build a two-workflow `LoadedDefinition` (root that `call:`s a child module owning `Assets`/`Skills`/the agent step) and store assets via `engine.StoreRunStartedAssetsForLoadedDefinition` (`engine/assets.go:40`) — that produces qualified keys naturally and exercises the real `call_step.go` module path. Prefer this if the synthetic-moduleID route is not exposed.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./engine/ -run TestRunAgentStep_SkillsQualifiedAssetKeyInSubWorkflow -v`
Expected: FAIL — `errArtifactFetch: skill library "awf" asset "skill_assets" was not recorded in run.started` (the bare lookup misses the qualified key).

- [ ] **Step 3: Apply the fix — thread `moduleID` and qualify the lookup**

Edit `engine/skills.go` `buildSkillCorpus` signature (`:71`):

```go
func buildSkillCorpus(id string, wf *ir.Workflow, moduleID string, assets map[string]RunStartedAsset, blobs state.Blobs) (*skillroute.Corpus, error) {
```

Edit the lookup (`:80-86`) to qualify the key:

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

Edit `selectAgentStepSkills` signature (`:23-31`) to add `moduleID string` after `path string`, and its call to `buildSkillCorpus` (`:32`):

```go
	corpus, err := buildSkillCorpus(as.Skills.From, wf, moduleID, runstate.Assets, blobs)
```

Edit the call site in `runAgentStepWithContext` (`engine/agent_step.go:88`):

```go
		selectedSkills, skillCorpus, oc, err = selectAgentStepSkills(as, path, ictx.moduleID, wf, runstate, log, blobs, scope)
```

(`QualifiedAssetKey`/`RootModuleID` are in-package — no new imports.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./engine/ -run TestRunAgentStep_SkillsQualifiedAssetKeyInSubWorkflow -v`
Expected: PASS. Also run the existing skills tests to confirm root (`moduleID==""`) is unaffected: `go test ./engine/ -run Skills -v` → PASS.

- [ ] **Step 5: Fix the stale doc comment** (optional, same edit set)

`engine/events.go:321-322` says "The map key … is the asset id" — append: ` (qualified moduleID/assetID for a sub-workflow asset).`

- [ ] **Step 6: Gate + commit**

Run: `make lint test`
Expected: PASS (0 failures).

```bash
git add engine/skills.go engine/agent_step.go engine/skills_run_test.go engine/skills_fixture_test.go engine/events.go
git commit -m "fix(engine/skills): qualify skill asset key by moduleID in sub-workflows" \
  -m "buildSkillCorpus looked up the skill-library directory asset by bare assetID; in a called sub-workflow run.started assets are keyed moduleID/assetID, so the lookup missed and halted with errArtifactFetch. Thread moduleID through selectAgentStepSkills->buildSkillCorpus and qualify via QualifiedAssetKey, mirroring resolveAssetInputFiles. Fixes P3. Root workflows (moduleID=\"\") unchanged." \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: P12 — stop AWF3002 firing on an agent-step gate evaluator

**Bug:** The AWF3002 loop (`ir/validate_refs.go:74-79`) flags any `kind=="agent"` producer with an `output_schema` and no inbound ref. A gate evaluator's terminal node is consumed ONLY via `{{ evaluate.<field> }}` (man:861, 1323), whose `checkRef` arm (`:677-683`) never marks the producer `referenced`. So an agent-step evaluator is a false positive. The fix must run in the `*Gate` arm of `walkRefs` (`:323-330`) — the only frame with both the gate node and `referenced` in scope (`checkRef` lacks the gate context, confirmed).

**Files:**
- Modify: `ir/validate_refs.go` (`*Gate` arm `:323-330`; add a helper near `indexModuleProducers` `:143`)
- Test: `ir/validate_refs_test.go` (new test; mirror `TestValidateRefsEvaluateScope` `:194-290` but with an `*AgentStep` evaluator)

**Interfaces:**
- Produces: `lastEvaluatorProducerID(nodes NodeList) string` — returns the terminal evaluator producer's step id, or `""`.

- [ ] **Step 1: Write the failing test** (agent-step gate evaluator consumed only via `{{ evaluate.verified }}`)

Add to `ir/validate_refs_test.go`:

```go
// TestRefsAgentGateEvaluatorNoAWF3002 covers P12: an agent-step gate evaluator
// whose typed output is consumed only via {{ evaluate.verified }} must NOT trip
// AWF3002 (it is structurally consumed, just not via a step.<id> ref). No
// existing test uses an *AgentStep evaluator — code-step evaluators never fire
// AWF3002 (kind != "agent"), which is why this went unnoticed.
func TestRefsAgentGateEvaluatorNoAWF3002(t *testing.T) {
	verifySchema := &JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"verified"},
		"properties":           map[string]any{"verified": map[string]any{"type": "boolean"}},
	}
	ld := makeLD(&Workflow{
		ID: "gate-eval", Version: 1,
		Containers: map[string]Container{"c": {Image: "oci://x@sha256:abc"}},
		Graph: NodeList{
			&Gate{
				Generate: NodeList{
					&CodeStep{ID: "gen1", Container: "c", Run: "gen"},
				},
				Evaluate: NodeList{
					&AgentStep{ID: "judge", Container: "c", Uses: "anthropic/claude-code",
						With: RawConfig{"prompt": "judge it"}, OutputSchema: verifySchema},
				},
				Until:       "{{ evaluate.verified }}",
				MaxAttempts: 3,
			},
		},
	})
	assertNoCode(t, Validate(ld), "AWF3002")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./ir/ -run TestRefsAgentGateEvaluatorNoAWF3002 -v`
Expected: FAIL — `assertNoCode` reports an AWF3002 diagnostic for step `judge`.

- [ ] **Step 3: Add the terminal-producer helper**

Add near `indexModuleProducers` in `ir/validate_refs.go`:

```go
// lastEvaluatorProducerID returns the step id of the terminal producer of a
// gate's evaluate list — the node whose typed output {{ evaluate.<field> }}
// resolves to at runtime (man: the final evaluate node must declare
// output_schema). Returns "" when the last node is not a direct producer step
// (e.g. nested control flow); such an evaluator terminal may still trip AWF3002
// — a known v1 limitation, acceptable because it is only a warning and the
// man-page requires the terminal to be a schema-declaring step.
func lastEvaluatorProducerID(nodes NodeList) string {
	if len(nodes) == 0 {
		return ""
	}
	switch v := nodes[len(nodes)-1].(type) {
	case *AgentStep:
		return v.ID
	case *CodeStep:
		return v.ID
	case *SignalStep:
		return v.ID
	default:
		return ""
	}
}
```

> **Implementer note:** confirm the concrete producer-step type names against the `indexModuleProducers` type switch (`ir/validate_refs.go:148-194`). `*AgentStep` and `*CodeStep` are confirmed (used in existing fixtures); add `*SignalStep`/`*CallStep` only if that switch lists them. AWF3002 only fires for `kind=="agent"`, so handling `*AgentStep` is load-bearing; the others are for correctness.

- [ ] **Step 4: Mark the evaluator terminal referenced in the `*Gate` arm**

Edit `ir/validate_refs.go:323-330`, append inside the `case *Gate:` block after the `walkRefs(v.Evaluate, …)` line:

```go
			walkRefs(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), c, producers, maps, referenced, false, "")
			// The evaluator's terminal typed output is consumed via
			// evaluate.<field>, which carries no step id; mark it referenced so
			// AWF3002 doesn't flag it as dead weight (P12).
			if id := lastEvaluatorProducerID(v.Evaluate); id != "" {
				referenced[id] = true
			}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./ir/ -run TestRefsAgentGateEvaluatorNoAWF3002 -v`
Expected: PASS. Run the full refs suite to confirm no regression (the AWF3002 positive test `TestRefsAgentSchemaWithoutRefWarnsAWF3002` must still fire, and the AWF5001/5003 gate-scope tests must stay green): `go test ./ir/ -run "Refs|EvaluateScope" -v` → PASS.

- [ ] **Step 6: Gate + commit**

Run: `make lint test`

```bash
git add ir/validate_refs.go ir/validate_refs_test.go
git commit -m "fix(ir): suppress AWF3002 on agent-step gate evaluators" \
  -m "A gate evaluator's typed output is consumed only via {{ evaluate.<field> }}, which carries no step id, so the AWF3002 reference pass never marked it referenced and flagged every agent-step evaluator as an unreferenced output_schema. Mark the evaluator's terminal producer referenced in the *Gate arm of walkRefs. Fixes P12. Known limitation: a non-step (nested control-flow) evaluator terminal may still warn." \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: WS-2a — surface the swallowed `map.item` body-failure cause

**Bug:** In `dispatchItem`, a body mechanical failure becomes a tolerated `item_failed` but `bodyErr`'s text is dropped — `commitMapItem` takes no cause param (`engine/map.go:496, 507-521`). The gate-in-map swallow (16 causeless `item_failed`s) is this. Add a forensic, bounded `Cause` field mirroring the existing `Outcome` field (event → fold → record), so the cause is durable and recoverable on resume. The fold never hashes event `Data`, so a free-text `Cause` is determinism-safe (confirmed).

**Files:**
- Modify: `engine/events.go` (`MapItemData` `:117-150`)
- Modify: `engine/map.go` (`dispatchItem` `:495-521`; `commitMapItem` sig `:529-531`; image_unavailable call site `:468`; add a `boundCause` helper)
- Modify: `engine/runstate.go` (`MapItemRecord` `:95`)
- Modify: `engine/fold.go` (map.item arm `:292-297`)
- Test: `engine/map_test.go` (new test; mirror `TestMapItemRecordsRetryableOutcome` `:1551-1574`)

**Interfaces:**
- Produces: `MapItemData.Cause string`; `MapItemRecord.Cause string`; `commitMapItem(log, runstate, mapPath, itemN, status, imageDigest, reason, outcome, cause string)`.

- [ ] **Step 1: Write the failing test** (a failing body must record a non-empty bounded `Cause` on the folded record)

Add to `engine/map_test.go` (mirror `TestMapItemRecordsRetryableOutcome`):

```go
// TestMapItemRecordsCause covers WS-2a: a body mechanical failure must record a
// bounded, human-readable cause on the folded map.item record (not just the
// mechanical Outcome) so a tolerated item_failed is no longer causeless.
func TestMapItemRecordsCause(t *testing.T) {
	rig := newMapRig(t, fail("echo a")) // exit 1 → body failure
	input := runOverItems("a")
	seedRunStartedWithInput(t, rig.lg, rig.blobs, input)
	minSuccess := ir.Ratio("1")
	wf := staticOverWorkflow("x", echoStep("x", &ir.RetryPolicy{Attempts: 1}), 1, &minSuccess)
	mapNode := wf.Graph[0].(*ir.Map)
	rs := NewRunState(testRunID, testDigest, input)

	_, _ = runMap(context.Background(), mapNode, testMapPath, wf, rs, rig.ld, rig.lg, rig.blobs, rig.clk, nil, nil)

	rs2 := foldFromRig(t, rig)
	items := rs2.LookupMapItems(testMapPath)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].Status != ItemFailed {
		t.Fatalf("Status = %q, want %q", items[0].Status, ItemFailed)
	}
	if items[0].Cause == "" {
		t.Errorf("Cause is empty; want the body failure cause recorded on the folded record")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./engine/ -run TestMapItemRecordsCause -v`
Expected: FAIL — `Cause is empty` (and a compile error first: `items[0].Cause` undefined — that is expected; add the field in Step 3).

- [ ] **Step 3: Add `Cause` to the event payload + bound the body error**

In `engine/events.go`, add to `MapItemData` (after `Outcome`, before the closing `}` at `:150`):

```go
	// Cause is a bounded, human-readable rendering of the item body's failure
	// (bodyErr) when Status == item_failed. Forensic only — empty for
	// item_passed / item_pruned and for pre-this-change logs (omitempty).
	// Distinct from Outcome (the mechanical class) and Reason (the infra cause).
	// Free-text and not equality-checked by the fold, so determinism-safe.
	Cause string `json:"cause,omitempty"`
```

In `engine/map.go`, add a deterministic bound helper (near the other map helpers):

```go
// maxMapItemCauseBytes bounds the forensic Cause string so a runaway body error
// can't bloat the journal. 1 KiB is ample for a one-line failure message.
const maxMapItemCauseBytes = 1024

func boundCause(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > maxMapItemCauseBytes {
		return s[:maxMapItemCauseBytes] + "…"
	}
	return s
}
```

In `dispatchItem` (`:495-521`), add a `cause` local and set it on the failure arm:

```go
	status := ItemPassed // default optimistic; revised below
	itemOutcome := ""    // set only on a body failure (spec §6.1)
	cause := ""          // bounded bodyErr text on a body failure (WS-2a)
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
		return status, nil
	}
	return commitMapItem(ictx.log, ictx.runstate, mapPath, itemN, status, imageDigest, "", itemOutcome, cause)
```

Update `commitMapItem` (`:529-531`) — add the `cause` param and field:

```go
func commitMapItem(log state.Log, runstate *RunState, mapPath string, itemN int, status, imageDigest, reason, outcome, cause string) (string, error) {
	data, mErr := json.Marshal(MapItemData{N: itemN, Status: status, ImageDigest: imageDigest, Reason: reason, Outcome: outcome, Cause: cause})
```

Update the image_unavailable call site (`:468`) with the new trailing `""`:

```go
	return commitMapItem(ictx.log, ictx.runstate, mapPath, itemN, ItemFailed, "", ReasonImageUnavailable, "", "")
```

- [ ] **Step 4: Thread `Cause` through the fold into `MapItemRecord`**

In `engine/runstate.go`, add to `MapItemRecord` (`:95`, alongside `Outcome`/`Reason`):

```go
	// Cause mirrors MapItemData.Cause (the bounded body-failure cause). Forensic
	// only; read from the FOLDED record like Outcome/Reason. Empty for
	// passed/pruned/image.
	Cause string
```

In `engine/fold.go` map.item arm (`:292-297`), add `Cause: d.Cause` to the `RecordMapItem` upsert:

```go
			rs.RecordMapItem(e.Path, MapItemRecord{
				N:           d.N,
				Status:      d.Status,
				Outcome:     d.Outcome,
				ImageDigest: d.ImageDigest,
				Reason:      d.Reason,
				Cause:       d.Cause,
				ItemValue:   itemValue, // keep whatever the existing arm binds
			})
```

> **Implementer note:** copy the existing field set verbatim from `engine/fold.go:292-297` and add the one `Cause: d.Cause` line — do not drop `ItemValue`/`N`/`Status` etc. Read the arm first.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./engine/ -run TestMapItemRecordsCause -v` → PASS.
Also confirm the existing outcome/fold tests still pass: `go test ./engine/ -run "MapItem|FoldMapItem" -v` → PASS.

- [ ] **Step 6: Gate + commit**

Run: `make lint test`

```bash
git add engine/events.go engine/map.go engine/runstate.go engine/fold.go engine/map_test.go
git commit -m "feat(engine/map): record the body-failure cause on map.item (stop the fan-in swallow)" \
  -m "A tolerated item_failed dropped bodyErr's text, leaving operators only the mechanical Outcome (the gate-in-map swallow that made the customer's 16 failures causeless). Add a bounded, forensic Cause field to MapItemData and MapItemRecord, captured in dispatchItem and folded like Outcome. Strengthens crash != verdict; determinism-safe (the fold never equality-checks event Data). WS-2a." \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: WS-2a (surface) — project the `map.item` cause in obs/trace

This is the observability surface for Task 3 (the journal already carries the cause; this makes `awf trace`/`inspect` show it). It is net-new (`obs/control_events.go` has no `map.item` case today — R6 deliberately omitted it). **Lower priority: if it proves thorny, it can land as its own commit later without blocking Tasks 1–3, 5.**

**Files:**
- Modify: `obs/control_events.go` (add `case engine.EventMapItem` to `attachControlEvents` `:23-63`; update the R6 comment `:19-22`)
- Modify: the obs attr-constants file (add `AttrMapItemCause`)
- Test: an `obs` projection test asserting the attribute lands on the failed item's span

- [ ] **Step 1: Locate the item span + add the attribute constant**

Read how `synthesizeScopes` builds the `map[i].item-N` child spans (per the R6 comment) and confirm `byPath[e.Path]` resolves to the item span for an `EventMapItem` whose `e.Path` is the map path. Add a new const next to `AttrGateOutcome` (grep `AttrGateOutcome` to find the attr file):

```go
const AttrMapItemCause = "awf.map.item.cause"
```

- [ ] **Step 2: Write the failing projection test**

Mirror an existing `obs` control-event test (grep `attachControlEvents` test usage). Assert that a folded log containing a `map.item` with a non-empty `Cause` produces a span carrying `AttrMapItemCause`.

- [ ] **Step 3: Run it red**, then **Step 4: add the projection case**:

```go
		case engine.EventMapItem:
			var d engine.MapItemData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return fmt.Errorf("obs.Project: map.item at %q: %w", e.Path, err)
			}
			if d.Cause == "" {
				continue
			}
			if s, ok := byPath[itemSpanPath(e.Path, d.N)]; ok {
				s.Attributes[AttrMapItemCause] = d.Cause
			}
```

> **Implementer note:** `itemSpanPath` is illustrative — use the SAME path helper `synthesizeScopes` uses to key the `map[i].item-N` child span (read it first; do not invent a path format). If the item child span is not in `byPath` at this point, attach to the map parent span instead and note it. Update the R6 comment (`:19-22`) to record that map.item now carries a forensic cause attribute.

- [ ] **Step 5: green + gate + commit**

Run: `go test ./obs/... -v` then `make lint test`.

```bash
git add obs/control_events.go obs/<attr-file>.go obs/<test-file>_test.go
git commit -m "feat(obs): project the map.item failure cause onto the item span" \
  -m "Surfaces WS-2a's forensic Cause in awf trace/inspect, reversing the R6 no-map.item-enrichment decision for the failure case only. WS-2a (surface)." \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: WS-2b — `awfllm` layered 429 classifier (permanent vs retryable)

**Goal:** Today `isPermanentLLMError` flags only `400 + invalid_request_error` as permanent; everything else (incl. a quota/budget-exhausted 429) is retryable, burning the retry budget. Add a layered 429 rule using response headers (`x-should-retry`, `Retry-After`) — which the SDK error already carries but `apiError` drops. `429` stays retryable EXCEPT when `x-should-retry: false` or the body type/code marks quota/budget exhaustion. Matches OpenAI/Anthropic SDK + RFC 9110 §15.6.4 / RFC 6585 §4.

**Scope note (grounded):** This targets `awfllm` (HTTP, headers reachable). The CLI adapters `codex`/`droid`/`goose` surface a free-text error string with no `*http.Response`; `codex` already escalates `400+invalid_request`→permanent (`agent/codex/result.go`), while `droid`/`goose` deliberately keep all runtime errors retryable (`agent/droid/stream.go:94`). Extending permanent-budget detection to the CLI adapters needs their empirical `budget_exceeded`/`insufficient_quota` message format (not in hand) and is a flagged follow-up — NOT in this task.

**Files:**
- Modify: `agent/awfllm/stream.go` (`apiError` `:26-30`; `isPermanentLLMError` `:32-50`)
- Modify: `agent/awfllm/transport.go` (`classifyOpenAIErr` `:333-343`)
- Test: `agent/awfllm/stream_test.go` (extend `TestIsPermanentLLMError`); `agent/awfllm/export_test.go` (extend `NewAPIErrorForTest`)

**Interfaces:**
- Produces: `apiError{Status int; Type string; Body string; ShouldRetry string; RetryAfter string}`; `NewAPIErrorForTest(status int, typ, body, shouldRetry, retryAfter string)`.

- [ ] **Step 1: Write the failing test** (429 + budget/quota or `x-should-retry: false` → permanent; plain 429 → retryable)

Replace `TestIsPermanentLLMError` in `agent/awfllm/stream_test.go`:

```go
func TestIsPermanentLLMError(t *testing.T) {
	perm := func(status int, typ, body, shouldRetry, retryAfter string) bool {
		return awfllm.IsPermanentLLMErrorForTest(awfllm.NewAPIErrorForTest(status, typ, body, shouldRetry, retryAfter))
	}
	if !perm(400, "invalid_request_error", "bad model", "", "") {
		t.Error("400 + invalid_request_error should be permanent")
	}
	if perm(429, "rate_limit_error", "slow down", "", "5") {
		t.Error("plain 429 rate-limit (Retry-After set) must be retryable")
	}
	if !perm(429, "insufficient_quota", "exceeded your current quota; check your plan and billing", "", "") {
		t.Error("429 insufficient_quota must be permanent")
	}
	if !perm(429, "rate_limit_error", "budget_exceeded", "false", "") {
		t.Error("429 with x-should-retry:false must be permanent")
	}
	if awfllm.IsPermanentLLMErrorForTest(errors.New("transport reset")) {
		t.Error("plain transport error must be retryable")
	}
}
```

Extend `NewAPIErrorForTest` in `agent/awfllm/export_test.go`:

```go
func NewAPIErrorForTest(status int, typ, body, shouldRetry, retryAfter string) *apiError {
	return &apiError{Status: status, Type: typ, Body: body, ShouldRetry: shouldRetry, RetryAfter: retryAfter}
}
```

- [ ] **Step 2: Run it red**

Run: `go test ./agent/awfllm/ -run TestIsPermanentLLMError -v`
Expected: FAIL/compile error — `apiError` has no `ShouldRetry`/`RetryAfter`; `NewAPIErrorForTest` arity changed.

- [ ] **Step 3: Add header fields + capture them in the mapping**

Edit `apiError` (`agent/awfllm/stream.go:26-30`):

```go
type apiError struct {
	Status      int
	Type        string
	Body        string
	ShouldRetry string // x-should-retry response header (OpenAI/Anthropic), "" if absent
	RetryAfter  string // Retry-After response header, "" if absent
}
```

Edit `classifyOpenAIErr` (`agent/awfllm/transport.go:333-343`) to capture the headers (`oe.Response` is `*http.Response`):

```go
func classifyOpenAIErr(err error) error {
	var oe *openai.Error
	if errors.As(err, &oe) {
		ae := &apiError{Status: oe.StatusCode, Type: oe.Type, Body: oe.Error()}
		if oe.Response != nil {
			ae.ShouldRetry = oe.Response.Header.Get("x-should-retry")
			ae.RetryAfter = oe.Response.Header.Get("Retry-After")
		}
		return ae
	}
	return err
}
```

- [ ] **Step 4: Add the layered rule to `isPermanentLLMError`**

Edit `agent/awfllm/stream.go:32-50`:

```go
// isPermanentLLMError reports a permanent client-side fault. Permanent when:
//   - the server says so: x-should-retry: false (authoritative when present); OR
//   - HTTP 400 + invalid_request_error (bad model / rejected schema); OR
//   - HTTP 429 whose body marks quota/budget exhaustion (insufficient_quota /
//     budget_exceeded), which retry can never clear.
// A plain 429 rate-limit (and 408/409/5xx, and non-apiError transport faults)
// stays retryable — the safe default bounded by the retry/gate budget.
func isPermanentLLMError(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.ShouldRetry == "false" {
		return true
	}
	if ae.Status == 400 && ae.Type == errTypeInvalidRequest {
		return true
	}
	if ae.Status == 429 && is429PermanentBody(ae.Type, ae.Body) {
		return true
	}
	return false
}

// is429PermanentBody reports a quota/budget-exhaustion 429 (vs a transient
// rate-limit). Status alone is ambiguous, so match the error type/code plus a
// small message-substring allowlist (brittle to provider wording — G3).
func is429PermanentBody(typ, body string) bool {
	if typ == "insufficient_quota" || typ == "budget_exceeded" {
		return true
	}
	for _, s := range []string{"insufficient_quota", "budget_exceeded", "exceeded your current quota", "check your plan and billing"} {
		if strings.Contains(body, s) {
			return true
		}
	}
	return false
}
```

(`strings` is already imported in `stream.go`.)

- [ ] **Step 5: Run it green**

Run: `go test ./agent/awfllm/ -run TestIsPermanentLLMError -v` → PASS.

- [ ] **Step 6: Gate + commit**

Run: `make lint test`

```bash
git add agent/awfllm/stream.go agent/awfllm/transport.go agent/awfllm/stream_test.go agent/awfllm/export_test.go
git commit -m "feat(agent/awfllm): classify quota/budget-exhausted 429 as permanent" \
  -m "isPermanentLLMError flagged only 400+invalid_request; a 429 from quota/budget exhaustion was retryable and burned the whole retry budget. Capture x-should-retry / Retry-After (dropped today) and add a layered 429 rule: permanent on x-should-retry:false or an insufficient_quota/budget_exceeded body; plain rate-limit 429 stays retryable. Matches OpenAI/Anthropic SDK + RFC 9110/6585. WS-2b. CLI-adapter (droid/goose) budget detection is a flagged follow-up." \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review (run after implementation)

- **Spec coverage:** WS-1 = Task 1 (P3) + Task 2 (P12). WS-2 = Task 3+4 (② map.item Cause journal+fold+obs) + Task 5 (② awfllm 429 classifier). The spec's "extend status→cause to droid/goose" is partially deferred (Task 5 scope note) — droid/goose budget detection needs their empirical error format; flagged, not silently dropped.
- **Placeholder scan:** the two `> Implementer note` blocks (Task 1 module harness, Task 4 item-span path) point at exact files to read because the harness/path detail wasn't extracted — they name the precise function to mirror, not "figure it out." Resolve them by reading the named site before coding.
- **Type consistency:** `Cause` is `string` everywhere (event, record, fold arm); `commitMapItem` gains exactly one trailing `cause string` param threaded to both call sites; `NewAPIErrorForTest` arity is updated in lockstep with `apiError`.

## Deferred (explicitly out of this plan)

- **CLI-adapter (droid/goose/codex) budget/quota → permanent.** Needs extraction of their launch/stream error paths + the empirical `budget_exceeded` message format. The customer's droid 429-budget case is *surfaced* by Task 3 (the cause is now visible) but not yet *reclassified* permanent on the droid path.
- **A full sub-workflow `imports:`+`call:` conformance bucket for P3** (Task 1 uses the tighter engine-level regression; the e2e bucket is a recommended follow-up under `conformance/`).
