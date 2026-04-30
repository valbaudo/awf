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
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, fooled_by_benign, feedback]
            properties:
              verified:         { type: boolean }
              fooled_by_benign: { type: boolean }
              feedback:         { type: string }
      until: "{{ evaluate.verified && !evaluate.fooled_by_benign }}"
      max_attempts: 2
`, fakeImageDigest)

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
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, fooled_by_benign, feedback]
            properties:
              verified:         { type: boolean }
              fooled_by_benign: { type: boolean }
              feedback:         { type: string }
      until: "{{ evaluate.verified && !evaluate.fooled_by_benign }}"
      max_attempts: 3
`, fakeImageDigest)

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
          output_schema:
            type: object
            additionalProperties: false
            required: [verified, fooled_by_benign, feedback]
            properties:
              verified:         { type: boolean }
              fooled_by_benign: { type: boolean }
              feedback:         { type: string }
      until: "{{ evaluate.verified && !evaluate.fooled_by_benign }}"
      max_attempts: 2
`, fakeImageDigest)

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
    output_schema:
      type: object
      additionalProperties: false
      required: [topic, sentiment]
      properties:
        topic:     { type: string }
        sentiment: { type: string }
`, fakeImageDigest)

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
