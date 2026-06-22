package conformance

import (
	"fmt"
	"strings"
)

// fakeImageDigest is the unverified-test image used by every fixture in this
// package. Pinned to a single constant so a future test-image bump only edits
// one site.
const fakeImageDigest = "oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// verdictSchemaYAML is the typed-verdict {verified, feedback} schema used by
// every gate fixture's evaluator step. Single source of truth — every gate
// fixture below embeds this block via fmt.Sprintf so a future verdict-shape
// change only edits here.
const verdictSchemaYAML = `output_schema:
            type: object
            additionalProperties: false
            required: [verified, feedback]
            properties:
              verified: { type: boolean }
              feedback: { type: string }`

// phase5VerdictSchemaYAML is the {verified, fooled_by_benign, feedback}
// schema used by Bucket 13's gate-agent evaluator. Distinct from
// verdictSchemaYAML which only has {verified, feedback} (used by Phase 3).
const phase5VerdictSchemaYAML = `output_schema:
            type: object
            additionalProperties: false
            required: [verified, fooled_by_benign, feedback]
            properties:
              verified:         { type: boolean }
              fooled_by_benign: { type: boolean }
              feedback:         { type: string }`

var tinySeqWorkflow = fmt.Sprintf(`workflow: conformance-tiny-seq
version: 1
containers:
  lab:
    image: %s
graph:
  - id: step1
    container: lab
    run: "./step1.sh"
  - id: step2
    container: lab
    run: "./step2.sh"
`, fakeImageDigest)

// fiveStepSeqWorkflow uses `retry: { attempts: 1 }` on every step — see
// slice 2.6 Design question 7. Bucket 2's `FailExecAfterN(k)` is one-shot
// per container.Fake's slice-2.4 contract; with retry.Default.Attempts=3
// the (k+2)-th call would succeed and the "crash" would be invisible.
// Pinning attempts=1 makes the fault actually halt the step.
var fiveStepSeqWorkflow = fmt.Sprintf(`workflow: conformance-five-step-seq
version: 1
containers:
  lab:
    image: %s
graph:
  - id: s1
    container: lab
    run: "./s1.sh"
    retry: { attempts: 1 }
  - id: s2
    container: lab
    run: "./s2.sh"
    retry: { attempts: 1 }
  - id: s3
    container: lab
    run: "./s3.sh"
    retry: { attempts: 1 }
  - id: s4
    container: lab
    run: "./s4.sh"
    retry: { attempts: 1 }
  - id: s5
    container: lab
    run: "./s5.sh"
    retry: { attempts: 1 }
`, fakeImageDigest)

var tinySeqWorkflowMutated = fmt.Sprintf(`workflow: conformance-tiny-seq
version: 1
containers:
  lab:
    image: %s
graph:
  - id: step1
    container: lab
    run: "./step1-MUTATED.sh"
  - id: step2
    container: lab
    run: "./step2.sh"
`, fakeImageDigest)

// propagationCaughtWorkflow exercises Bucket 4a sub-test "caught": step2 is
// wrapped in try.catch.finally; step2's command always exits 1 (with
// retry: { attempts: 1 } so the one-shot fake fault halts the step on the
// first call). The catch absorbs the failure, finally runs, step3 runs, the
// run completes ok.
var propagationCaughtWorkflow = fmt.Sprintf(`workflow: conformance-propagation-caught
version: 1
containers:
  lab:
    image: %s
graph:
  - id: step1
    container: lab
    run: "./step1.sh"
    retry: { attempts: 1 }
  - try:
      do:
        - id: step2-failing
          container: lab
          run: "./step2-failing.sh"
          retry: { attempts: 1 }
      catch:
        - id: catch-step
          container: lab
          run: "./catch.sh"
          retry: { attempts: 1 }
      finally:
        - id: finally-step
          container: lab
          run: "./finally.sh"
          retry: { attempts: 1 }
  - id: step3
    container: lab
    run: "./step3.sh"
    retry: { attempts: 1 }
`, fakeImageDigest)

// propagationUncaughtWorkflow is identical to propagationCaughtWorkflow
// EXCEPT step2 is NOT wrapped in try.catch — its failure propagates to the
// run root.
var propagationUncaughtWorkflow = fmt.Sprintf(`workflow: conformance-propagation-uncaught
version: 1
containers:
  lab:
    image: %s
graph:
  - id: step1
    container: lab
    run: "./step1.sh"
    retry: { attempts: 1 }
  - id: step2-failing
    container: lab
    run: "./step2-failing.sh"
    retry: { attempts: 1 }
  - id: step3
    container: lab
    run: "./step3.sh"
    retry: { attempts: 1 }
`, fakeImageDigest)

// skipAtRootWorkflow exercises Bucket 6 sub-test "at_root": a single Skip
// node at the workflow root. The validator accepts an empty `containers: {}`
// for workflows where no step references a container — skip never invokes
// the dispatcher, so no container is needed (verified empirically).
const skipAtRootWorkflow = `workflow: conformance-skip-root
version: 1
containers: {}
graph:
  - skip: "early exit"
`

// skipInLoopBodyWorkflow exercises Bucket 6 sub-test "in_loop_body":
// loop{max_iters:3, body:[skip]} — each iter ends via skip, loop runs all
// 3 iters, 3 loop.iter + 3 node.skipped recorded. No container needed (the
// body never reaches a step that uses one).
const skipInLoopBodyWorkflow = `workflow: conformance-skip-loop
version: 1
containers: {}
graph:
  - loop:
      max_iters: 3
      body:
        - skip: "skip iter"
`

// skipInTryDoWorkflow exercises Bucket 6 sub-test "in_try_do": skip inside
// try.do bypasses Catch, runs Finally, and — per spec §5.6 — propagates past
// the try to the workflow root, preventing siblings AFTER the try from running.
// Steps that must NOT run are deliberately unprogrammed in the fake: the
// ProgramExec-miss error would fire and surface the spec violation.
var skipInTryDoWorkflow = fmt.Sprintf(`workflow: conformance-skip-try
version: 1
containers:
  lab:
    image: %s
graph:
  - try:
      do:
        - skip: "skip do"
      catch:
        - id: must-not-run-catch
          container: lab
          run: "./must-not-run-catch.sh"
          retry: { attempts: 1 }
      finally:
        - id: must-run-finally
          container: lab
          run: "./must-run-finally.sh"
          retry: { attempts: 1 }
  - id: must-not-run-after-try
    container: lab
    run: "./must-not-run-after-try.sh"
    retry: { attempts: 1 }
`, fakeImageDigest)

// parallelCancellationWorkflow — Bucket 4b parallel_cancellation: try.catch
// wraps a 3-branch parallel. Branch 0 fails (retry-exhausting); branches
// 1+2 are each try { do, finally }. Distinct containers (AWF1010).
var parallelCancellationWorkflow = fmt.Sprintf(`workflow: conformance-parallel-cancel
version: 1
containers:
  c0:
    image: %[1]s
  c1:
    image: %[1]s
  c2:
    image: %[1]s
graph:
  - try:
      do:
        - parallel:
            - id: b0-failing
              container: c0
              run: "./b0-failing.sh"
              retry: { attempts: 1 }
            - try:
                do:
                  - id: b1-do
                    container: c1
                    run: "./b1-do.sh"
                    retry: { attempts: 1 }
                finally:
                  - id: b1-finally
                    container: c1
                    run: "./b1-finally.sh"
                    retry: { attempts: 1 }
            - try:
                do:
                  - id: b2-do
                    container: c2
                    run: "./b2-do.sh"
                    retry: { attempts: 1 }
                finally:
                  - id: b2-finally
                    container: c2
                    run: "./b2-finally.sh"
                    retry: { attempts: 1 }
      catch:
        - id: outer-catch
          container: c0
          run: "./outer-catch.sh"
          retry: { attempts: 1 }
`, fakeImageDigest)

// gateFeedbackThreadingWorkflow — Bucket 5 feedback_threading. Generator's
// run command interpolates {{ evaluate.feedback }}; the fake's ProgramExec
// can only program one result per command, so the test asserts dispatch
// SEQUENCE (attempt 1 with empty feedback, attempt 2 with "missing X" from
// attempt-1's verdict). max_attempts: 2 so both attempts run, both reject
// (eval returns same verdict each call), gate exhausts → OutcomeRejected.
// Pass-on-2 verdict-roundtrip is covered by the engine-level test.
var gateFeedbackThreadingWorkflow = fmt.Sprintf(`workflow: conformance-gate-feedback
version: 1
containers:
  c0:
    image: %s
graph:
  - gate:
      generate:
        - id: gen1
          container: c0
          run: "./gen.sh {{ evaluate.feedback }}"
          retry: { attempts: 1 }
      evaluate:
        - id: eval1
          container: c0
          run: "./eval.sh"
          retry: { attempts: 1 }
          %s
      until: "{{ evaluate.verified }}"
      max_attempts: 2
`, fakeImageDigest, verdictSchemaYAML)

// gateMaxAttemptsRejectedWorkflow — Bucket 5 max_attempts_rejected. Evaluator
// always returns verified:false; gate exhausts max_attempts:3 attempts and
// returns OutcomeRejected.
var gateMaxAttemptsRejectedWorkflow = fmt.Sprintf(`workflow: conformance-gate-max-attempts
version: 1
containers:
  c0:
    image: %s
graph:
  - gate:
      generate:
        - id: gen1
          container: c0
          run: "./gen.sh"
          retry: { attempts: 1 }
      evaluate:
        - id: eval1
          container: c0
          run: "./eval.sh"
          retry: { attempts: 1 }
          %s
      until: "{{ evaluate.verified }}"
      max_attempts: 3
`, fakeImageDigest, verdictSchemaYAML)

// gateCrashNotVerdictWorkflow — Bucket 5 crash_not_verdict. Generator
// crashes (retry-exhausted) on attempt 1; gate must propagate WITHOUT
// committing a gate.attempt event. The evaluator script entry is deliberately
// programmed to verified:true — if it ran (it should NOT), the gate would
// pass on attempt 1 and the test would mis-judge as "no rejection."
var gateCrashNotVerdictWorkflow = fmt.Sprintf(`workflow: conformance-gate-crash
version: 1
containers:
  c0:
    image: %s
graph:
  - gate:
      generate:
        - id: gen1
          container: c0
          run: "./gen-crash.sh"
          retry: { attempts: 1 }
      evaluate:
        - id: eval1
          container: c0
          run: "./eval.sh"
          retry: { attempts: 1 }
          %s
      until: "{{ evaluate.verified }}"
      max_attempts: 3
`, fakeImageDigest, verdictSchemaYAML)

// gateMidResumeWorkflow — Bucket 5 mid_resume. max_attempts:5. Evaluator
// always returns verified:false on the first run, then is re-programmed to
// also keep failing on resume (5 total attempts → rejected). The test asserts
// the post-run + post-resume RunState has GateAttempts of length 5, with N
// monotonically increasing 1..5 across the run boundary.
var gateMidResumeWorkflow = fmt.Sprintf(`workflow: conformance-gate-mid-resume
version: 1
containers:
  c0:
    image: %s
graph:
  - gate:
      generate:
        - id: gen1
          container: c0
          run: "./gen.sh"
          retry: { attempts: 1 }
      evaluate:
        - id: eval1
          container: c0
          run: "./eval.sh"
          retry: { attempts: 1 }
          %s
      until: "{{ evaluate.verified }}"
      max_attempts: 5
`, fakeImageDigest, verdictSchemaYAML)

// gateIndependencePlaceholderWorkflow — Bucket 5 independence_placeholder.
// Trivial gate (2-step generate, 1-step evaluator). The test asserts the
// recordingDispatcher captured 3 DISTINCT NodeIntent paths (gen1, gen2, eval1)
// — the Phase 5 fresh-context proof replaces this assertion when the
// agent.Adapter lands.
var gateIndependencePlaceholderWorkflow = fmt.Sprintf(`workflow: conformance-gate-independence
version: 1
containers:
  c0:
    image: %s
graph:
  - gate:
      generate:
        - id: gen1
          container: c0
          run: "./gen1.sh"
          retry: { attempts: 1 }
        - id: gen2
          container: c0
          run: "./gen2.sh"
          retry: { attempts: 1 }
      evaluate:
        - id: eval1
          container: c0
          run: "./eval.sh"
          retry: { attempts: 1 }
          %s
      until: "{{ evaluate.verified }}"
      max_attempts: 1
`, fakeImageDigest, verdictSchemaYAML)

// gateRejectedCaughtWorkflow — Bucket 5 rejected_caught_by_try (critique-pass
// addition). Gate wrapped in try.catch; gate rejects (eval returns verified:false,
// max_attempts:1); catch absorbs; handler step runs; run completes ok.
//
// This fixture nests the gate one extra level deep (under try.do), so the
// verdictSchemaYAML sub-lines need an additional 6 spaces of leading indent
// compared to the top-level gate fixtures. strings.ReplaceAll re-indents
// the shared schema constant so it stays in sync with the other 5 fixtures.
var gateRejectedCaughtWorkflow = fmt.Sprintf(`workflow: conformance-gate-rejected-caught
version: 1
containers:
  c0:
    image: %s
graph:
  - try:
      do:
        - gate:
            generate:
              - id: gen1
                container: c0
                run: "./gen.sh"
                retry: { attempts: 1 }
            evaluate:
              - id: eval1
                container: c0
                run: "./eval.sh"
                retry: { attempts: 1 }
                %s
            until: "{{ evaluate.verified }}"
            max_attempts: 1
      catch:
        - id: handler
          container: c0
          run: "./handler.sh"
          retry: { attempts: 1 }
`, fakeImageDigest, strings.ReplaceAll(verdictSchemaYAML, "\n            ", "\n                  "))

// mapStandardWorkflow — used by Bucket 7 map_per_item_commits AND
// map_resume_skips_committed_items sub-tests. 3 items, body runs
// `./process.sh {{ x }}` per item; default min_success (= all 3).
// Per design §E: 3 map.item events committed at addressable per-item paths
// (map[0].item-0.process etc).
const mapStandardWorkflow = `workflow: conformance-map-standard
version: 1
input:
  type: object
  required: [items]
  additionalProperties: false
  properties:
    items:
      type: array
      items: { type: string }
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - map:
      over: "{{ input.items }}"
      as: x
      container: c0
      concurrency: 2
      body:
        - id: process
          container: c0
          run: "./process.sh {{ x }}"
          retry: { attempts: 1 }
`

// mapSkipInItemWorkflow — Bucket 7 map_skip_in_item_records_passed. Body is an
// if-statement that runs `skip:` on item == "b" else echoes. Pins design §E
// step 5: skip ends the item as item_passed.
const mapSkipInItemWorkflow = `workflow: conformance-map-skip-in-item
version: 1
input:
  type: object
  required: [items]
  additionalProperties: false
  properties:
    items:
      type: array
      items: { type: string }
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - map:
      over: "{{ input.items }}"
      as: x
      container: c0
      concurrency: 3
      body:
        - if:
            cond: "{{ x == \"b\" }}"
            then:
              - skip: "skip middle item"
            else:
              - id: process
                container: c0
                run: "./process.sh {{ x }}"
                retry: { attempts: 1 }
`

// mapAggregationWorkflow — Bucket 17 (Phase 5 slice 5.5). Map A scans each
// input item into a typed {finding, index} output; map B fans out over A's
// aggregated findings via `over: "{{ step.scan }}"` — the index-ordered array
// of A's committed per-item `scan` outputs, each element bound as `f`. Map B's
// body consumes `{{ f.finding }}` (object item → field access; NO `.value`).
//
// concurrency: 1 on both maps. Aggregation is concurrency-independent, and the
// in-memory fake's shared Blobs race when multiple output_schema item bodies
// commit concurrently (a known fake limitation) — serializing sidesteps it
// without affecting what this bucket pins.
const mapAggregationWorkflow = `workflow: conformance-map-aggregation
version: 1
input:
  type: object
  required: [items]
  additionalProperties: false
  properties:
    items:
      type: array
      items: { type: string }
containers:
  c0:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - map:
      over: "{{ input.items }}"
      as: x
      container: c0
      concurrency: 1
      body:
        - id: scan
          container: c0
          run: "./scan.sh {{ x }}"
          retry: { attempts: 1 }
          output_schema:
            type: object
            additionalProperties: false
            required: [finding, index]
            properties:
              finding: { type: string }
              index:   { type: integer }
  - map:
      over: "{{ step.scan }}"
      as: f
      container: c0
      concurrency: 1
      body:
        - id: verify
          container: c0
          run: "./verify.sh {{ f.finding }}"
          retry: { attempts: 1 }
`

// agentStepBasicWorkflow — Bucket 12. One AgentStep with templated `with` +
// output_schema. Bucket sub-test programs the fake adapter to return a
// specific typed verdict; asserts the typed output round-trips through
// node.completed.
var agentStepBasicWorkflow = fmt.Sprintf(`workflow: conformance-agent-step
version: 1
containers:
  lab:
    image: %s
graph:
  - id: triage
    container: lab
    uses: anthropic/claude-code
    with:
      prompt: "do the thing"
    output_schema:
      type: object
      additionalProperties: false
      required: [verdict, confidence]
      properties:
        verdict: { type: string }
        confidence: { type: number }
`, fakeImageDigest)

// gateAgentPassOnAttempt1Workflow — Bucket 13a. Generator: agent ref
// "test/gen" returns {exploit: "real"} on attempt 0. Evaluator: agent ref
// "test/oracle" returns {verified: true} on attempt 0. Gate passes.
var gateAgentPassOnAttempt1Workflow = fmt.Sprintf(`workflow: conformance-gate-agent-pass
version: 1
containers:
  lab:
    image: %s
graph:
  - gate:
      generate:
        - id: gen1
          container: lab
          uses: test/gen
          with:
            prompt: "build the exploit"
          output_schema:
            type: object
            additionalProperties: false
            required: [exploit]
            properties:
              exploit: { type: string }
      evaluate:
        - id: eval1
          container: lab
          uses: test/oracle
          with:
            prompt: "verify exploit"
          %s
      until: "{{ evaluate.verified && !evaluate.fooled_by_benign }}"
      max_attempts: 2
`, fakeImageDigest, phase5VerdictSchemaYAML)

// gateAgentRepairOnAttempt2Workflow — Bucket 13b. Same shape but generator's
// prompt includes a feedback template: "{{ evaluate.feedback }}". Attempt 1:
// fake oracle returns {verified: false, fooled_by_benign: true, feedback: "..."}.
// Attempt 2: oracle returns {verified: true, ...}. Gate passes on attempt 2.
// The test asserts BOTH the gate's pass outcome AND that the generator's
// AgentInvocation.With on attempt 2 contains the substituted feedback.
var gateAgentRepairOnAttempt2Workflow = fmt.Sprintf(`workflow: conformance-gate-agent-repair
version: 1
containers:
  lab:
    image: %s
graph:
  - gate:
      generate:
        - id: gen1
          container: lab
          uses: test/gen
          with:
            prompt: "build the exploit. previous feedback: {{ evaluate.feedback }}"
          output_schema:
            type: object
            additionalProperties: false
            required: [exploit]
            properties:
              exploit: { type: string }
      evaluate:
        - id: eval1
          container: lab
          uses: test/oracle
          with:
            prompt: "verify exploit"
          %s
      until: "{{ evaluate.verified && !evaluate.fooled_by_benign }}"
      max_attempts: 3
`, fakeImageDigest, phase5VerdictSchemaYAML)

// gateAgentMaxAttemptsRejectedWorkflow — Bucket 13c. Oracle ALWAYS returns
// verified:false. Gate exhausts max_attempts:2 → rejected.
var gateAgentMaxAttemptsRejectedWorkflow = fmt.Sprintf(`workflow: conformance-gate-agent-rejected
version: 1
containers:
  lab:
    image: %s
graph:
  - gate:
      generate:
        - id: gen1
          container: lab
          uses: test/gen
          with:
            prompt: "build the exploit"
          output_schema:
            type: object
            additionalProperties: false
            required: [exploit]
            properties:
              exploit: { type: string }
      evaluate:
        - id: eval1
          container: lab
          uses: test/oracle
          with:
            prompt: "verify exploit"
          %s
      until: "{{ evaluate.verified && !evaluate.fooled_by_benign }}"
      max_attempts: 2
`, fakeImageDigest, phase5VerdictSchemaYAML)

// gateAgentThreadSubConversationWorkflow — T2 (continues:). A sub-conversation
// INSIDE one gate's generate: [ask, refine continues: ask]. The evaluator
// rejects attempt 1, passes attempt 2, so ask runs twice (A1 then A2). refine's
// assembled Thread in the passing attempt must carry ATTEMPT-2's ask transcript
// (A2) and NOT attempt-1's (A1) — stepRuntimePath resolves the continues
// predecessor to refine's OWN attempt. Rejected attempts still commit; they are
// excluded by ADDRESSING (same-attempt resolution), not by skipping the write.
//
// refine declares an output_schema only so the bucket has a typed generate
// output to point at; nothing references refine.draft, so AWF3002 emits a
// (harmless) warning — identical to the gen1 fixtures above. The gate's verdict
// comes from the LAST evaluate node (judge / test/oracle), not from the
// generate steps.
var gateAgentThreadSubConversationWorkflow = fmt.Sprintf(`workflow: conformance-gate-agent-thread
version: 1
containers:
  lab:
    image: %s
graph:
  - gate:
      generate:
        - id: ask
          container: lab
          uses: test/llm
          with:
            prompt: "ask"
        - id: refine
          container: lab
          uses: test/llm
          continues: ask
          with:
            prompt: "refine"
          output_schema:
            type: object
            additionalProperties: false
            required: [draft]
            properties:
              draft: { type: string }
      evaluate:
        - id: judge
          container: lab
          uses: test/oracle
          with:
            prompt: "judge"
          %s
      until: "{{ evaluate.verified && !evaluate.fooled_by_benign }}"
      max_attempts: 3
`, fakeImageDigest, phase5VerdictSchemaYAML)

// gatedLeafThreadWorkflow — T4 (evaluator independence). draft + critique are
// plain prior turns; revise (the gate's generate leaf) continues critique;
// judge (the evaluator) has NO continues. Asserts judge's invocation Thread
// is empty — the evaluator judges in a fresh context (D8, gate integrity).
var gatedLeafThreadWorkflow = fmt.Sprintf(`workflow: conformance-gated-leaf-thread
version: 1
containers:
  lab:
    image: %s
graph:
  - id: draft
    container: lab
    uses: test/llm
    with: { prompt: "draft" }
  - id: critique
    container: lab
    uses: test/llm
    continues: draft
    with: { prompt: "critique" }
  - gate:
      generate:
        - id: revise
          container: lab
          uses: test/llm
          continues: critique
          with: { prompt: "revise" }
          output_schema:
            type: object
            additionalProperties: false
            required: [draft]
            properties:
              draft: { type: string }
      evaluate:
        - id: judge
          container: lab
          uses: test/oracle
          with: { prompt: "judge" }
          %s
      until: "{{ evaluate.verified && !evaluate.fooled_by_benign }}"
      max_attempts: 2
`, fakeImageDigest, phase5VerdictSchemaYAML)

// gatedEvaluatorContextEvidenceWorkflow verifies that an evaluator may declare
// continues: for upstream source evidence while still receiving no active Thread.
var gatedEvaluatorContextEvidenceWorkflow = fmt.Sprintf(`workflow: conformance-gated-evaluator-context-evidence
version: 1
containers:
  lab:
    image: %s
graph:
  - id: draft
    container: lab
    uses: test/llm
    with:
      model: m
      prompt: draft
    output_schema:
      type: object
      additionalProperties: false
  - id: critique
    container: lab
    uses: test/llm
    continues: draft
    with:
      model: m
      prompt: critique
    output_schema:
      type: object
      additionalProperties: false
  - gate:
      max_attempts: 1
      generate:
        - id: revise
          container: lab
          uses: test/llm
          continues: critique
          with:
            model: m
            prompt: revise
          output_schema:
            type: object
            additionalProperties: false
            required: [draft]
            properties:
              draft: { type: string }
      evaluate:
        - id: judge
          container: lab
          uses: test/llm
          continues: critique
          retry: { attempts: 1 }
          with:
            model: m
            prompt: judge
          %s
      until: "{{ evaluate.verified && !evaluate.fooled_by_benign }}"
`, fakeImageDigest, phase5VerdictSchemaYAML)

// signalAwaitWorkflow — Bucket 8 signal_await_delivers + signal_resume_replays.
// A single `await: human_review` step followed by an echo step that references
// the signal payload. No containers entry for the await itself; the after step
// runs in container c.
var signalAwaitWorkflow = fmt.Sprintf(`workflow: signal-await
version: 1
containers:
  c:
    image: %s
graph:
  - id: approve
    await: human_review
    output_schema:
      type: object
      additionalProperties: false
      required: [approved]
      properties:
        approved: { type: boolean }
  - id: after
    container: c
    run: echo "{{ step.approve.approved }}"
`, fakeImageDigest)

// signalWhereWorkflow — SP4 keyed-signal conformance. A map over two hypotheses;
// each item's body awaits `oob-hit` WHERE candidate_id matches the item's id, so
// two buffered signals are correlated to the right item regardless of seq order.
// The await step needs no container; the map needs a container for its body scope.
var signalWhereWorkflow = fmt.Sprintf(`workflow: signal-where
version: 1
input:
  type: object
  required: [hyps]
  additionalProperties: false
  properties:
    hyps:
      type: array
      items:
        type: object
        required: [id]
        additionalProperties: false
        properties:
          id: { type: string }
containers:
  c:
    image: %s
graph:
  - map:
      over: input.hyps
      as: hyp
      container: c
      concurrency: 2
      body:
        - id: wait_oob
          await: oob-hit
          where: 'candidate_id == "{{ hyp.id }}"'
          timeout: 2s   # fast-fail bound: a mis-correlated item never matches and
                        # times out (retryable_failure) in 2s instead of hanging the
                        # suite until the global test timeout. Both signals are
                        # pre-buffered, so a correctly-correlated item matches on the
                        # first poll, well inside this bound.
          output_schema:
            type: object
            additionalProperties: false
            required: [candidate_id, hit]
            properties:
              candidate_id: { type: string }
              hit: { type: boolean }
`, fakeImageDigest)

// signalPauseWorkflow — Bucket 8 signal_pause_halts + signal_cancel_terminal.
// Three simple sequential echo steps; the signal subsystem halts the run
// before all steps complete (pause) or terminally (cancel).
var signalPauseWorkflow = fmt.Sprintf(`workflow: signal-pause
version: 1
containers:
  c:
    image: %s
graph:
  - id: a
    container: c
    run: echo a
  - id: b
    container: c
    run: echo b
  - id: c2
    container: c
    run: echo c
`, fakeImageDigest)

// layer2ContractWorkflow — Bucket 15. Same shape as Bucket 12 but the
// adapter declares Caps{NativeSchema: false}. The fixture is identical;
// the bucket distinguishes by adapter capability, not workflow shape.
var layer2ContractWorkflow = fmt.Sprintf(`workflow: conformance-layer2-contract
version: 1
containers:
  lab:
    image: %s
graph:
  - id: extract
    container: lab
    uses: test/non-native-schema
    with:
      prompt: "extract structured data from prose"
    retry:
      attempts: 3
    output_schema:
      type: object
      additionalProperties: false
      required: [topic, sentiment]
      properties:
        topic:     { type: string }
        sentiment: { type: string }
`, fakeImageDigest)

// agentStepContainerlessWorkflow — Part A. One AgentStep with NO container and
// a typed output_schema, served by a Containerless fake. Proves the engine
// commits a containerless agent step (and resumes it from the log).
const agentStepContainerlessWorkflow = `workflow: conformance-agent-containerless
version: 1
graph:
  - id: ask
    uses: awf/llm
    with:
      model: m
      prompt: "what is 6 times 7"
    output_schema:
      type: object
      additionalProperties: false
      required: [answer]
      properties:
        answer: { type: string }
`

// agentStepContainerlessInputFilesWorkflow — Task 12. One CONTAINERLESS
// agent step that receives a PDF via input_files. The file is provided at run
// start as a single-file ASSET (doc.pdf, written next to the workflow); the
// step forwards it to the containerless adapter under the logical label "doc".
//
// Deviation (documented in the test): the engine resolves input.files.<name>
// (workflow input file) and asset.<id> through the SAME containerless resolver
// (engine.resolveContainerlessInputFiles -> resolveSingleRefBytes -> DetectMIME),
// but only the asset.<id> run-start channel is wired end-to-end through
// engine.Run + the fake-backend harness. input.files.<name> at the top level is
// reachable only by constructing a Scope directly (engine's
// input_files_containerless_test.go covers that), so the conformance harness
// drives the same delivery path via asset.<id>.
const agentStepContainerlessInputFilesWorkflow = `workflow: conformance-agent-containerless-input-files
version: 1
assets:
  doc: doc.pdf
graph:
  - id: ask
    uses: awf/llm
    with:
      model: m
      prompt: "Summarize the attached document."
    input_files:
      doc: asset.doc
    output_schema:
      type: object
      additionalProperties: false
      required: [answer]
      properties:
        answer: { type: string }
`

// parallelResumeWorkflow — Bucket 4b parallel_resume_consistency:
// simple 3-branch parallel followed by a sequential after-step. The test
// programs pb2.sh to fail deterministically on first run, then re-programs
// it to succeed on resume. Expectation: pb0+pb1 commit pre-crash, pb2
// doesn't; resume skips committed (replays from log), re-runs only pb2 +
// after.
var parallelResumeWorkflow = fmt.Sprintf(`workflow: conformance-parallel-resume
version: 1
containers:
  c0:
    image: %[1]s
  c1:
    image: %[1]s
  c2:
    image: %[1]s
graph:
  - parallel:
      - id: pb0
        container: c0
        run: "./pb0.sh"
        retry: { attempts: 1 }
      - id: pb1
        container: c1
        run: "./pb1.sh"
        retry: { attempts: 1 }
      - id: pb2
        container: c2
        run: "./pb2.sh"
        retry: { attempts: 1 }
  - id: after
    container: c0
    run: "./after.sh"
    retry: { attempts: 1 }
`, fakeImageDigest)

// threadFanOutWorkflow — T10. A common pre-fork ancestor `seed` lives OUTSIDE
// the parallel; all three branches continues: seed AND share the identical
// system_prompt. Per E.2.a the assembled cached region (system_prompt + thread)
// is byte-identical across branches; only with.prompt (the tail) differs. The
// fake does not cache — this asserts the byte-identity PRECONDITION, not a hit.
// A per-branch system_prompt would diverge the prefix at byte 0; that negative
// is a documented caveat (E.2.a/§6), not asserted (the fake cannot model it).
var threadFanOutWorkflow = fmt.Sprintf(`workflow: conformance-thread-fanout
version: 1
containers:
  seed_c:
    image: %[1]s
  c_a:
    image: %[1]s
  c_b:
    image: %[1]s
  c_c:
    image: %[1]s
graph:
  - id: seed
    container: seed_c
    uses: test/chat
    with:
      system_prompt: "SHARED-SYSTEM-PROMPT"
      prompt: "establish shared context"
  - parallel:
      - id: branch_a
        container: c_a
        uses: test/chat
        continues: seed
        with:
          system_prompt: "SHARED-SYSTEM-PROMPT"
          prompt: "branch A tail"
      - id: branch_b
        container: c_b
        uses: test/chat
        continues: seed
        with:
          system_prompt: "SHARED-SYSTEM-PROMPT"
          prompt: "branch B tail"
      - id: branch_c
        container: c_c
        uses: test/chat
        continues: seed
        with:
          system_prompt: "SHARED-SYSTEM-PROMPT"
          prompt: "branch C tail"
`, fakeImageDigest)

// threadBranchedWorkflow — T9 runtime half. draft → critique → if/else.
// BOTH forks continue: critique (a valid, dominating link: critique precedes
// the if and encloses neither fork). cond is static-true so the `then` fork is
// taken deterministically; the taken fork's turn must receive critique's
// thread = [draft-pair, critique-pair]. system_prompt is shared so the run is
// realistic, but T9 asserts the THREAD, not byte-identity (that is T10).
var threadBranchedWorkflow = fmt.Sprintf(`workflow: conformance-thread-branched
version: 1
containers:
  lab:
    image: %s
graph:
  - id: draft
    container: lab
    uses: test/chat
    with:
      system_prompt: "you are a helpful assistant"
      prompt: "draft a plan"
  - id: critique
    container: lab
    uses: test/chat
    continues: draft
    with:
      system_prompt: "you are a helpful assistant"
      prompt: "critique the draft"
  - if:
      cond: "{{ 1 == 1 }}"
      then:
        - id: revise_then
          container: lab
          uses: test/chat
          continues: critique
          with:
            system_prompt: "you are a helpful assistant"
            prompt: "revise per the critique (then-fork)"
      else:
        - id: revise_else
          container: lab
          uses: test/chat
          continues: critique
          with:
            system_prompt: "you are a helpful assistant"
            prompt: "revise per the critique (else-fork)"
`, fakeImageDigest)

// artifactChannelWorkflow is the SP1 artifact-channel fixture (Bucket: artifacts).
// A producer `recon` in container `lab` declares a NAMED output_files artifact
// `report`; a consumer `hunt` in a DISTINCT container `box` stages it via
// input_files at a different in-container path. This pins the cross-container
// handoff (content-addressed, resume-safe) the man page's "Artifact channel"
// section promises. retry: { attempts: 1 } on both so the run is deterministic
// against the one-shot fake.
var artifactChannelWorkflow = fmt.Sprintf(`workflow: conformance-artifacts
version: 1
containers:
  lab:
    image: %[1]s
  box:
    image: %[1]s
graph:
  - id: recon
    container: lab
    run: "./recon.sh"
    retry: { attempts: 1 }
    output_files: { report: /out/report.md }
  - id: hunt
    container: box
    run: "./hunt.sh"
    retry: { attempts: 1 }
    input_files: { /work/report.md: step.recon.files.report }
`, fakeImageDigest)
