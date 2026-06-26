package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/retry"
	"github.com/valbaudo/awf/skillroute"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

// runAgentStep is the interpreter-side handler for *ir.AgentStep — symmetric
// to runCodeStep. It:
//
//  1. Builds a template.Scope from RunState + the step's runtime path. This
//     same scope is what resolves {{ evaluate.<field> }} when the step sits
//     inside a gate's generate block (Phase 3.3 wiring; see baseline note
//     in the plan's Task 8 prose).
//  2. Substitutes string-leaf values in AgentStep.With via template.Substitute.
//  3. Substitutes AgentStep.IdempotencyKey (if any).
//  4. Builds NodeIntent and calls engine.RunWithRetry.
//  5. Writes one agent.event log entry per buffered AgentEvent (Blobs offload
//     for payloads ≥ AgentEventInlineThreshold).
//  6. Calls the canonical engine.Commit (engine/commit.go) and records the
//     resulting NodeResult in runstate.
//
// Per CLAUDE.md "interpreter is the only writer to state": Log.Append calls
// are made HERE (and via the canonical Commit), not in the dispatcher. Per
// "simplest solution first": the commit logic is NOT duplicated — slice 5.2
// reuses engine.Commit verbatim.
func runAgentStepWithContext(ctx context.Context, as *ir.AgentStep, path string, ictx interpreterContext) (Outcome, error) {
	wf := ictx.wf
	runstate := ictx.runstate
	dispatcher := ictx.dispatcher
	log := ictx.log
	blobs := ictx.blobs
	clk := ictx.clk
	tap := ictx.tap

	// Resume short-circuit (mirrors runCodeStep — committed nodes are replayed,
	// not recomputed).
	if _, ok := runstate.LookupCompleted(path); ok {
		return OutcomeOK, nil
	}

	scope := ictx.scope(path)

	// 1. Substitute With.
	resolvedWith, err := substituteRawConfig(as.With, scope)
	if err != nil {
		return failStep(log, path, OutcomePermanentFailure, fmt.Errorf("engine.runAgentStep: substitute with at %q: %w", path, err))
	}

	// 2. Substitute idempotency_key (if any).
	var idempotencyKey string
	if as.IdempotencyKey != nil {
		idempotencyKey, err = template.Substitute(string(*as.IdempotencyKey), scope)
		if err != nil {
			return failStep(log, path, OutcomePermanentFailure, fmt.Errorf("engine.runAgentStep: substitute idempotency_key at %q: %w", path, err))
		}
	}

	// 3. Build retry policy. Verified pattern from runCodeStep
	// (engine/interpreter.go:271): retry.Merge returns (retry.Policy, error);
	// an error here is an author bug in the workflow's retry: block.
	policy, err := retry.Merge(retry.Default, as.Retry)
	if err != nil {
		return "", fmt.Errorf("engine.runAgentStep: build retry policy at path %q: %w", path, err)
	}

	if as.Container == "" && as.Skills != nil {
		return failStep(log, path, OutcomePermanentFailure,
			fmt.Errorf("engine.runAgentStep: skills requires a container; agent step %q is containerless", as.ID))
	}

	var selectedSkills SkillsSelectedData
	var skillCorpus *skillroute.Corpus
	if as.Skills != nil {
		var oc Outcome
		selectedSkills, skillCorpus, oc, err = selectAgentStepSkills(as, path, wf, ictx.moduleID, runstate, log, blobs, scope)
		if err != nil {
			err = fmt.Errorf("engine.runAgentStep: select skills at %q: %w", path, err)
			if oc == OutcomePermanentFailure {
				return failStep(log, path, OutcomePermanentFailure, err)
			}
			return "", err
		}
	}

	// Slice 5.3: populate Feedback from the enclosing gate's prior verdict.
	// When `path` is inside a gate's `.generate.` subtree, the same gate-path
	// resolver used by template `{{ evaluate.<field> }}` (engine/gate_path.go)
	// gives us the gate's path; from runstate.LookupGateAttempts we read the
	// most-recent committed verdict. nil on attempt 1 (no prior verdict yet),
	// nil for non-gate paths.
	//
	// Anti-aliasing note: the map copy below is INTENTIONAL — `last.Verdict`
	// is a pointer to the live runstate.GateAttempts map. Aliasing it into
	// `feedback` would let downstream callers (the adapter, a future template
	// substitution path) mutate runstate by mistake. The copy keeps Feedback
	// owned by this step's invocation, runstate immutable. Cheap — verdicts
	// are small (typed schema-validated outputs, ~10-100 fields).
	var feedback ir.RawConfig
	if gatePath, ok := enclosingGateForEvaluate(path); ok {
		attempts := runstate.LookupGateAttempts(gatePath)
		if len(attempts) > 0 {
			last := attempts[len(attempts)-1]
			if len(last.Verdict) > 0 {
				feedback = ir.RawConfig{}
				for k, v := range last.Verdict {
					feedback[k] = v
				}
			}
		}
	}

	// continues: threading — assemble the prior turns from the committed log.
	// Pure over the committed log: stepPathIndex is a deterministic whole-graph
	// walk (memoized once per run via sync.Once on RunState); stepRuntimePath
	// resolves each predecessor to THIS consumer's own attempt/item/iter
	// (ctxPath == path), which is why rejected gate attempts and foreign map
	// items are excluded by addressing, not special-casing. One Scope is reused
	// across the walk. Walked root->current (prepend), so the oldest turn is
	// first and the immediate predecessor is last. Outside gate.evaluate these
	// turns become Thread; inside gate.evaluate they become ContextEvidence so
	// adapters cannot accidentally render source evidence as active judge history.
	var continuedTurns []agent.ThreadTurn
	idx := runstate.stepPathIndex(wf)
	byID := runstate.agentStepByID(wf)
	for cur := as.Continues; cur != ""; {
		predRuntime, perr := scope.stepRuntimePath(idx[cur])
		if perr != nil { // impossible after validation (AWF1027/AWF1031); defensive.
			return failStep(log, path, OutcomePermanentFailure,
				fmt.Errorf("engine.runAgentStep: resolve continues target %q at %q: %w", cur, path, perr))
		}
		predNR, ok := runstate.LookupCompleted(predRuntime)
		if !ok { // ok guaranteed by dominance (AWF1027); defensive.
			return failStep(log, path, OutcomePermanentFailure,
				fmt.Errorf("engine.runAgentStep: continues target %q not committed (runtime %q)", cur, predRuntime))
		}
		if predNR.Transcript == (agent.ThreadTurn{}) {
			return failStep(log, path, OutcomePermanentFailure,
				fmt.Errorf("engine.runAgentStep: continues target %q has no committed transcript (runtime %q)", cur, predRuntime))
		}
		continuedTurns = append([]agent.ThreadTurn{{User: predNR.Transcript.User, Assistant: predNR.Transcript.Assistant}}, continuedTurns...)
		if tgt, ok2 := byID[cur]; ok2 {
			cur = tgt.Continues
		} else {
			cur = ""
		}
	}
	var thread []agent.ThreadTurn
	var contextEvidence []agent.ThreadTurn
	if isGateEvaluateContext(path) {
		contextEvidence = continuedTurns
	} else {
		thread = continuedTurns
	}

	// Containerless agent steps deliver input_files to the adapter as inline
	// message parts (agent.InputFile) instead of staging into a container.
	var containerlessFiles []agent.InputFile
	if as.Container == "" {
		containerlessFiles, err = resolveContainerlessInputFiles(as.InputFiles, scope, wf, ictx.moduleID, blobs, runstate.Assets)
		if err != nil {
			if errors.Is(err, errArtifactFetch) {
				return "", fmt.Errorf("engine.runAgentStep: stage input_files at %q: %w", path, err)
			}
			return failStep(log, path, OutcomePermanentFailure, err)
		}
	}

	// Resolve input_files to staged bytes (SP1) for the CONTAINER path. Same
	// errArtifactFetch classification as runCodeStep: a ref error
	// (parse/undeclared/not-committed) is an author bug → permanent_failure; a
	// Blobs.Get failure of a committed, content-addressed artifact is
	// corruption/IO → internal halt ("" outcome), so resume re-runs the
	// uncommitted step and re-fetches. A containerless step has nowhere to stage
	// into, so it skips this block entirely (handled above as inline parts).
	var inputFiles []container.InputFile
	if as.Container != "" {
		inputFileEntries, err := resolveInputFileEntries(as.InputFiles, scope, wf, ictx.moduleID, blobs, runstate.Assets)
		if err != nil {
			if errors.Is(err, errArtifactFetch) {
				return "", fmt.Errorf("engine.runAgentStep: stage input_files at %q: %w", path, err)
			}
			return failStep(log, path, OutcomePermanentFailure, err)
		}
		if as.Skills != nil {
			skillFiles, err := resolveSelectedSkillFiles(selectedSkills, skillCorpus, string(as.Skills.Into))
			if err != nil {
				return failStep(log, path, OutcomePermanentFailure, err)
			}
			inputFileEntries = append(inputFileEntries, skillFiles...)
		}
		inputFiles, err = inputFilesFromResolvedEntries(inputFileEntries)
		if err != nil {
			return failStep(log, path, OutcomePermanentFailure, err)
		}
	}

	outputFiles, outputFileContracts, err := resolveOutputFiles(as.OutputFiles, scope, ictx.moduleID, runstate.Assets, blobs)
	if err != nil {
		if errors.Is(err, errArtifactFetch) {
			return "", fmt.Errorf("engine.runAgentStep: resolve output_files contracts at %q: %w", path, err)
		}
		return failStep(log, path, OutcomePermanentFailure, fmt.Errorf("engine.runAgentStep: substitute output_files at %q: %w", path, err))
	}

	// 4. Build ResolvedInputs. Timeout cast follows the runCodeStep idiom
	// (engine/interpreter.go:283): ir.AgentStep.Timeout is *ir.Duration where
	// `type Duration time.Duration`, so the deref-then-cast is the conversion.
	snapBare, _ := SplitContainerRef(as.Container)
	uses := AgentRuntimeRef(wf, ictx.moduleID, as.Uses)

	// Per-run config isolation + session capture. A container-backed adapter that
	// declares IsolatedConfigDir gets a RunID-keyed per-run config directory
	// (<staging-root>/claude-session/<run-id>) injected as CLAUDE_CONFIG_DIR, so
	// concurrent runs never collide on shared host config. If it ALSO declares
	// PersistentSession, the engine additionally captures/restores its session as
	// that dir's `projects/` subtree (the transcript bucket lives INSIDE the
	// captured subtree, so claude's cwd / with.workdir no longer participate). A
	// Containerless adapter (e.g. agent/codexlive) has no container filesystem to
	// isolate or capture and is excluded — including it would fire tree ops against
	// a zero handle and fail every successful run. Both paths are RunID-keyed and
	// RunID is read-from-log on resume, so the dirs are determinism-safe.
	var sessionDir, sessionConfigDirRel string
	if ictx.resolver != nil {
		if adp, ok := ictx.resolver.Lookup(uses); ok {
			caps := adp.Capabilities()
			if !caps.Containerless && caps.IsolatedConfigDir {
				sessionConfigDirRel = dispatcherStagingRoot(ictx.dispatcher) + "/claude-session/" + runstate.RunID
				if caps.PersistentSession {
					sessionDir = sessionConfigDirRel + "/projects"
				}
			}
		}
	}

	resolved := ResolvedInputs{
		Uses:                  uses,
		With:                  resolvedWith,
		OutputFiles:           outputFiles,
		OutputSchema:          as.OutputSchema,
		NonRetryableExitCodes: policy.NonRetryableExitCodes,
		Snapshot:              wf.Containers[snapBare].Snapshot,
		Feedback:              feedback, // slice 5.3
		Thread:                thread,   // Task 4.5
		ContextEvidence:       contextEvidence,
		InputFiles:            inputFiles,         // SP1 artifact channel (container path)
		ContainerlessFiles:    containerlessFiles, // inline message parts (containerless path)
		OutputFileContracts:   outputFileContracts,
		SessionConfigDirRel:   sessionConfigDirRel, // RunID-keyed per-run CLAUDE_CONFIG_DIR (staging form); IsolatedConfigDir adapters
		SessionDir:            sessionDir,          // projects/ subtree to capture; PersistentSession adapters only
	}
	if as.Timeout != nil {
		resolved.Timeout = time.Duration(*as.Timeout)
	}

	dispatchCtx, cancelDispatch := context.WithCancel(ctx)
	defer cancelDispatch()
	sink := newAgentEventSink(dispatchCtx, cancelDispatch, clk, log, blobs, path)

	intent := NodeIntent{
		Path:           path,
		Node:           as,
		ResolvedInputs: resolved,
		IdempotencyKey: idempotencyKey,
		IsGateEvaluate: isGateEvaluateContext(path),
		RunContext: agent.RunContext{
			RunID:        runstate.RunID,
			CurrentEpoch: runstate.Epoch,
			NextEpoch:    runstate.Epoch,
		},
		agentEventSink: sink,
	}

	appendNodeStarted(log, path, "agent")

	dr, chunks, runErr := RunWithRetry(dispatchCtx, dispatcher, intent, policy, clk, log)
	// Drain via the canonical helper. Agent steps' chunks channel is the
	// pre-closed one runAgent returns, so this is a no-op on the agent path,
	// but using drainTap keeps the dispatch tail symmetric with runCodeStep
	// (engine/interpreter.go:298) and inherits its defensive nil-handling +
	// tap-write-failure suppression.
	drainTap(chunks, as.ID, tap)

	// 4. Write agent.event log entries from the buffered events. Done BEFORE
	// the commit so the journal records them adjacent to the node.completed
	// (happy path) or node.failed (failure path). On failure, runErr is
	// authoritative — appendErr is reported only when runErr is nil so we
	// never silently mask a step failure with an internal append error.
	sinkErr := sink.closeWait()
	appendErr := appendAgentEvents(log, blobs, path, dr.AgentEvents)

	// 5. Failure paths: mirror runCodeStep's split (engine/interpreter.go:309-316).
	// dr.Outcome == "" means the dispatcher never dispatched (unknown container,
	// ErrUnsupportedKind). That's an INTERNAL error — no node.failed entry,
	// no fold corruption on resume. dr.Outcome != "" means the step ran and
	// failed — failStep writes node.failed with the underlying cause.
	if runErr != nil {
		if sinkErr != nil {
			return "", fmt.Errorf("engine.runAgentStep: live agent.event sink at %q: %w", path, sinkErr)
		}
		if dr.Outcome == "" {
			return "", fmt.Errorf("engine.runAgentStep: dispatch at path %q: %w", path, runErr)
		}
		return failStep(log, path, dr.Outcome, runErr)
	}

	// On happy path, surface any earlier appendAgentEvents failure now.
	if sinkErr != nil {
		return "", fmt.Errorf("engine.runAgentStep: live agent.event sink at %q: %w", path, sinkErr)
	}
	if appendErr != nil {
		return "", fmt.Errorf("engine.runAgentStep: append agent.event entries at %q: %w", path, appendErr)
	}

	// 6. Happy path: commit via the canonical engine.Commit. Commit owns the
	// content-address-then-pointer-swap invariant (CLAUDE.md "Commit"); we
	// reuse it verbatim. Then mirror the result into runstate. A step
	// participates in a conversation (so its transcript blob must be committed)
	// iff it is continued-from by some other step (i.e. it is a thread target).
	// A leaf turn (continues: someone, but nobody continues IT) NEVER needs its
	// transcript committed: the thread-assembly loop only reads transcripts of
	// targets, never of the consumer itself. Committing a leaf transcript wastes
	// Blobs storage with data nothing ever reads.
	participates := runstate.threadTargets(wf)[as.ID]
	nr, commitErr := Commit(log, blobs, path, dr, participates)
	if commitErr != nil {
		return "", fmt.Errorf("engine.runAgentStep: commit at %q: %w", path, commitErr)
	}
	runstate.RecordCompleted(path, nr)
	if dr.Live != nil && ictx.liveFinalizer != nil {
		if finalErr := ictx.liveFinalizer(ctx, *dr.Live); finalErr != nil {
			msg := finalErr.Error()
			if tap != nil {
				_, _ = fmt.Fprintf(tap, "· live finalizer: %s\n", liveDisplayField(msg))
			}
			_ = appendAgentEvents(log, blobs, path, []agent.AgentEvent{{
				Kind:    "live.finalizer.warning",
				Stream:  "stderr",
				Live:    true,
				Payload: []byte(msg),
				Display: agent.EventDisplay{Class: agent.DisplayNotice, Text: msg},
			}})
		}
	}
	return OutcomeOK, nil
}

// substituteRawConfig walks the With map and applies template.Substitute to
// every string leaf. Non-string values (numbers, booleans, nested objects)
// are not substituted in slice 5.2 — only top-level string fields (matching
// what AWF spec §7 templating supports: "substitution = fill references
// before a command runs"). Nested-object substitution lands when a real
// adapter demands it.
//
// Returns a freshly-allocated map; never mutates the input.
func substituteRawConfig(in ir.RawConfig, scope template.Scope) (ir.RawConfig, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(ir.RawConfig, len(in))
	for k, v := range in {
		s, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		sub, err := template.Substitute(s, scope)
		if err != nil {
			return nil, fmt.Errorf("substitute %q: %w", k, err)
		}
		out[k] = sub
	}
	return out, nil
}

// appendAgentEvents writes one agent.event log entry per buffered AgentEvent.
// Payloads ≥ AgentEventInlineThreshold are offloaded to Blobs and the entry
// carries the CAS pointer; smaller payloads are inline.
func appendAgentEvents(log state.Log, blobs state.Blobs, path string, events []agent.AgentEvent) error {
	for _, ev := range events {
		eventPayload := ev.Payload
		data := AgentEventData{
			Kind:   ev.Kind,
			Stream: ev.Stream,
		}
		if ev.Live {
			eventPayload = []byte(agent.RedactDisplayText(agent.SanitizeDisplayBytes(eventPayload)))
			data.Live = true
			data.DisplayClass = ev.Display.Class.String()
			data.DisplayTool = liveDisplayField(ev.Display.Tool)
			data.DisplaySummary = liveDisplayField(ev.Display.Text)
			data.DisplayLines = ev.Display.Lines
			data.DisplayBytes = ev.Display.Bytes
			data.DisplayIsError = ev.Display.IsError
		}
		data.Size = len(eventPayload)
		if len(eventPayload) >= AgentEventInlineThreshold {
			ref, err := blobs.Put(eventPayload)
			if err != nil {
				return fmt.Errorf("Blobs.Put agent.event payload: %w", err)
			}
			data.PayloadRef = ref
		} else {
			data.PayloadInline = eventPayload
		}
		payload, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("marshal AgentEventData: %w", err)
		}
		if err := log.Append(state.Event{Type: EventAgentEvent, Path: path, Data: payload}); err != nil {
			return fmt.Errorf("Log.Append agent.event: %w", err)
		}
	}
	return nil
}

func liveDisplayField(s string) string {
	return boundDisplayField(agent.RedactDisplayText(agent.SanitizeDisplayText(s)), agentEventDisplayFieldLimit)
}

func boundDisplayField(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := 0
	for i, r := range s {
		next := i + utf8.RuneLen(r)
		if next > limit {
			break
		}
		cut = next
	}
	if cut == 0 {
		return fmt.Sprintf("...[truncated %d bytes]", len(s))
	}
	return s[:cut] + fmt.Sprintf("...[truncated %d bytes]", len(s)-cut)
}
