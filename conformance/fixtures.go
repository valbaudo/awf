package conformance

const tinySeqWorkflow = `workflow: conformance-tiny-seq
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: step1
    container: lab
    run: "./step1.sh"
  - id: step2
    container: lab
    run: "./step2.sh"
`

// fiveStepSeqWorkflow uses `retry: { attempts: 1 }` on every step — see
// slice 2.6 Design question 7. Bucket 2's `FailExecAfterN(k)` is one-shot
// per container.Fake's slice-2.4 contract; with retry.Default.Attempts=3
// the (k+2)-th call would succeed and the "crash" would be invisible.
// Pinning attempts=1 makes the fault actually halt the step.
const fiveStepSeqWorkflow = `workflow: conformance-five-step-seq
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
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
`

const tinySeqWorkflowMutated = `workflow: conformance-tiny-seq
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - id: step1
    container: lab
    run: "./step1-MUTATED.sh"
  - id: step2
    container: lab
    run: "./step2.sh"
`

// propagationCaughtWorkflow exercises Bucket 4a sub-test "caught": step2 is
// wrapped in try.catch.finally; step2's command always exits 1 (with
// retry: { attempts: 1 } so the one-shot fake fault halts the step on the
// first call). The catch absorbs the failure, finally runs, step3 runs, the
// run completes ok.
const propagationCaughtWorkflow = `workflow: conformance-propagation-caught
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
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
`

// propagationUncaughtWorkflow is identical to propagationCaughtWorkflow
// EXCEPT step2 is NOT wrapped in try.catch — its failure propagates to the
// run root.
const propagationUncaughtWorkflow = `workflow: conformance-propagation-uncaught
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
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
`

// skipAtRootWorkflow exercises Bucket 6 sub-test "at_root": a single Skip
// node at the workflow root. The validator (Phase 1.4) requires containers
// to be non-empty in some scenarios; this fixture declares a placeholder
// container even though no step uses it — keeps the loader happy.
const skipAtRootWorkflow = `workflow: conformance-skip-root
version: 1
containers:
  unused:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - skip: "early exit"
`

// skipInLoopBodyWorkflow exercises Bucket 6 sub-test "in_loop_body":
// loop{max_iters:3, body:[skip]} — each iter ends via skip, loop runs all
// 3 iters, 3 loop.iter + 3 node.skipped recorded.
const skipInLoopBodyWorkflow = `workflow: conformance-skip-loop
version: 1
containers:
  unused:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - loop:
      max_iters: 3
      body:
        - skip: "skip iter"
`

// skipInTryDoWorkflow exercises Bucket 6 sub-test "in_try_do": skip inside
// try.do bypasses Catch, runs Finally, propagates ok. Catch contains a step
// that — if run — fails the test (the fake has NO program for it; the
// ProgramExec-miss error fires).
const skipInTryDoWorkflow = `workflow: conformance-skip-try
version: 1
containers:
  lab:
    image: oci://example.com/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
graph:
  - try:
      do:
        - skip: "skip do"
      catch:
        - id: must-not-run
          container: lab
          run: "./must-not-run.sh"
          retry: { attempts: 1 }
      finally:
        - id: must-run
          container: lab
          run: "./must-run.sh"
          retry: { attempts: 1 }
`
