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
`
