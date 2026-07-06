package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/awf/agent"
	agentfake "github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/state"
)

type callRunRig struct {
	fake  *container.Fake
	disp  *LocalDispatcher
	log   *state.InMemoryLog
	blobs *state.InMemoryBlobs
	clk   *clock.Fake
	rs    *RunState
}

func newCallRunRig(t *testing.T) *callRunRig {
	t.Helper()
	clk := &clock.Fake{T: time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)}
	fake := container.NewFake()
	return &callRunRig{
		fake:  fake,
		disp:  &LocalDispatcher{Backend: fake, Handles: map[string]container.Handle{}},
		log:   state.NewInMemoryLog(clk),
		blobs: state.NewInMemoryBlobs(),
		clk:   clk,
		rs:    NewRunState("run-call", "digest-call", nil),
	}
}

func (r *callRunRig) seedRunStarted(t *testing.T) {
	t.Helper()
	if err := r.log.Append(state.Event{
		Type: EventRunStarted,
		Data: marshalOrFatal(t, RunStartedData{RunID: r.rs.RunID, WorkflowDigest: r.rs.WorkflowDigest}),
	}); err != nil {
		t.Fatalf("seed run.started: %v", err)
	}
}

func TestRunCallStepCommitsCallProduct(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rig.fake.ProgramExec("summarize CVE-2026-0001", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"summary":"child result"}`),
	}, nil)
	def := callLoadedDefinition(
		&ir.Workflow{
			ID:      "root",
			Version: 1,
			Imports: map[string]string{
				"scan": "scan.awf.yaml",
			},
			Graph: ir.NodeList{
				&ir.CallStep{ID: "recon", Call: "scan", Input: map[string]ir.TemplateValue{
					"topic": json.RawMessage(`"CVE-2026-0001"`),
				}},
			},
		},
		childSummaryWorkflow("scan", "summarize {{ input.topic }}"),
	)

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if got := rig.rs.Completed["recon"].Outputs["summary"]; got != "child result" {
		t.Fatalf("call product summary = %v, want child result", got)
	}
	if _, ok := rig.rs.Completed["recon.workflow.summarize"]; !ok {
		t.Fatalf("missing child completion at recon.workflow.summarize")
	}
	events := callEvents(t, rig.log)
	assertEventOrder(t, events, []eventAtPath{
		{typ: EventNodeStarted, path: "recon"},
		{typ: EventCallStarted, path: "recon"},
		{typ: EventNodeStarted, path: "recon.workflow.summarize"},
		{typ: EventNodeCompleted, path: "recon.workflow.summarize"},
		{typ: EventNodeCompleted, path: "recon"},
	})
}

func TestRunCallStepPersistsCallStartedBeforeChild(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	def := simpleCallDefinition(childSummaryWorkflow("scan", "unprogrammed child"))

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if oc != OutcomeRetryableFailure {
		t.Fatalf("Outcome = %q, want %q (err=%v)", oc, OutcomeRetryableFailure, err)
	}
	if _, ok := rig.rs.LookupCallStarted("recon"); !ok {
		t.Fatalf("CallStarted missing for recon after child failure")
	}
	events := callEvents(t, rig.log)
	assertEventOrder(t, events, []eventAtPath{
		{typ: EventNodeStarted, path: "recon"},
		{typ: EventCallStarted, path: "recon"},
		{typ: EventNodeStarted, path: "recon.workflow.summarize"},
		{typ: EventNodeFailed, path: "recon.workflow.summarize"},
		{typ: EventNodeFailed, path: "recon"},
	})
}

func TestRunCallStepResumeAfterCallStartedSpanOnly(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	if err := rig.log.Append(state.Event{
		Type: EventNodeStarted,
		Path: "recon",
		Data: marshalOrFatal(t, NodeStartedData{Kind: "call"}),
	}); err != nil {
		t.Fatalf("seed node.started: %v", err)
	}
	rig.fake.ProgramExec("summarize child", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"summary":"after resume"}`),
	}, nil)

	oc, err := Run(context.Background(), simpleCallDefinition(childSummaryWorkflow("scan", "summarize child")), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	events := callEvents(t, rig.log)
	if got := countEvents(events, EventCallStarted, "recon"); got != 1 {
		t.Fatalf("call.started count = %d, want 1", got)
	}
}

func TestRunCallStepResumeUsesRecordedInput(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	inputRef, err := rig.blobs.Put([]byte(`{"topic":"recorded"}`))
	if err != nil {
		t.Fatalf("seed call input: %v", err)
	}
	if err := rig.log.Append(state.Event{
		Type: EventCallStarted,
		Path: "recon",
		Data: marshalOrFatal(t, CallStartedData{InputRef: inputRef}),
	}); err != nil {
		t.Fatalf("seed call.started: %v", err)
	}
	folded, err := Fold(mustFoldEvents(t, rig.log), rig.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	rig.rs = folded
	rig.fake.ProgramExec("summarize recorded", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"summary":"recorded input used"}`),
	}, nil)
	def := callLoadedDefinition(
		&ir.Workflow{
			ID:      "root",
			Version: 1,
			Imports: map[string]string{
				"scan": "scan.awf.yaml",
			},
			Graph: ir.NodeList{
				&ir.CallStep{ID: "recon", Call: "scan", Input: map[string]ir.TemplateValue{
					"topic": json.RawMessage(`"mutated"`),
				}},
			},
		},
		childSummaryWorkflow("scan", "summarize {{ input.topic }}"),
	)

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if len(rig.fake.Calls) != 1 || rig.fake.Calls[0].Run != "summarize recorded" {
		t.Fatalf("child command = %+v, want exactly summarize recorded", rig.fake.Calls)
	}
	if got := countEvents(callEvents(t, rig.log), EventCallStarted, "recon"); got != 1 {
		t.Fatalf("call.started count = %d, want existing single record reused", got)
	}
}

func TestRunCallStepChildFailureFailsCallBoundary(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rig.fake.ProgramExec("summarize child", container.ExecResult{ExitCode: 1}, nil)

	oc, err := Run(context.Background(), simpleCallDefinition(childSummaryWorkflow("scan", "summarize child")), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if oc != OutcomeRetryableFailure {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeRetryableFailure)
	}
	if err == nil {
		t.Fatalf("Run err = nil, want child failure")
	}
	events := callEvents(t, rig.log)
	childIdx := eventIndex(events, EventNodeFailed, "recon.workflow.summarize")
	callIdx := eventIndex(events, EventNodeFailed, "recon")
	if childIdx < 0 || callIdx < 0 || childIdx > callIdx {
		t.Fatalf("failure order child=%d call=%d events=%+v", childIdx, callIdx, events)
	}
	var failed NodeFailedData
	if err := json.Unmarshal(events[callIdx].Data, &failed); err != nil {
		t.Fatalf("unmarshal call node.failed: %v", err)
	}
	if !strings.Contains(failed.Error, "recon.workflow.summarize") {
		t.Fatalf("call boundary error = %q, want child path", failed.Error)
	}
}

func TestRunCallStepChildInternalErrorDoesNotFailCallBoundary(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Graph: ir.NodeList{&ir.CallStep{ID: "recon", Call: "scan"}},
	}
	child := &ir.Workflow{
		ID:      "scan",
		Version: 1,
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "broken", Container: "missing", Run: "never"},
		},
	}

	oc, err := Run(context.Background(), callLoadedDefinition(root, child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty for internal child error", oc)
	}
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v, want missing-container internal error", err)
	}
	if got := countEvents(callEvents(t, rig.log), EventNodeFailed, "recon"); got != 0 {
		t.Fatalf("parent call node.failed count = %d, want 0 for child internal error", got)
	}
}

func TestRunCallStepDestroysPartialChildHandlesOnCreateFailure(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rig.disp.Backend = &failSecondCreateBackend{Fake: rig.fake}
	child := &ir.Workflow{
		ID:      "scan",
		Version: 1,
		Containers: map[string]ir.Container{
			"a": {Image: "oci://ok@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			"b": {Image: "oci://fail@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "work", Container: "a", Run: "never"},
		},
	}

	oc, err := Run(context.Background(), simpleCallNoInputDefinition(child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if oc != OutcomeRetryableFailure {
		t.Fatalf("Outcome = %q, want %q (err=%v)", oc, OutcomeRetryableFailure, err)
	}
	if err == nil {
		t.Fatalf("Run err = nil, want create failure")
	}
	if len(rig.fake.DestroyCalls) != 1 {
		t.Fatalf("DestroyCalls len = %d, want 1 for partial child handle cleanup", len(rig.fake.DestroyCalls))
	}
	if rig.fake.DestroyCalls[0].Name != "recon.workflow::a" {
		t.Fatalf("destroyed handle name = %q, want recon.workflow::a", rig.fake.DestroyCalls[0].Name)
	}
}

func TestRunCallStepChildStepRefsResolveWithinChildWorkflow(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rig.fake.ProgramExec("produce", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"value":"child-value"}`),
	}, nil)
	rig.fake.ProgramExec("consume child-value", container.ExecResult{ExitCode: 0}, nil)
	child := &ir.Workflow{
		ID:         "scan",
		Version:    1,
		Containers: map[string]ir.Container{"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "produce", Container: "c", Run: "produce", OutputSchema: awfStringObjectSchema("value")},
			&ir.CodeStep{ID: "consume", Container: "c", Run: "consume {{ step.produce.value }}"},
		},
	}

	oc, err := Run(context.Background(), simpleCallNoInputDefinition(child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if len(rig.fake.Calls) != 2 || rig.fake.Calls[1].Run != "consume child-value" {
		t.Fatalf("fake calls = %+v, want second command consume child-value", rig.fake.Calls)
	}
}

func TestRunCallStepChildLocalRoleDispatchesWithChildRoleConfig(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	base := agentfake.New("test/base").
		WithCaps(agent.Caps{NativeSchema: true, Containerless: true}).
		Script(0, agentfake.Result{Output: map[string]any{"summary": "child"}})
	var reg agent.Registry
	if err := reg.Register(base); err != nil {
		t.Fatalf("Register base: %v", err)
	}

	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Agents: map[string]ir.AgentRole{
			"auditor": {Uses: "test/base", With: ir.RawConfig{"scope": "root"}},
		},
		Graph: ir.NodeList{&ir.CallStep{ID: "recon", Call: "scan"}},
	}
	child := &ir.Workflow{
		ID:      "scan",
		Version: 1,
		Agents: map[string]ir.AgentRole{
			"auditor": {Uses: "test/base", With: ir.RawConfig{"scope": "child"}},
		},
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "audit", Uses: "auditor"},
		},
	}
	if err := reg.Register(agent.NewDerivedAdapter(AgentRuntimeRef(root, "", "auditor"), base, root.Agents["auditor"].With)); err != nil {
		t.Fatalf("Register root role: %v", err)
	}
	childRef := AgentRuntimeRef(child, "mod-scan", "auditor")
	if err := reg.Register(agent.NewDerivedAdapter(childRef, base, child.Agents["auditor"].With)); err != nil {
		t.Fatalf("Register child role: %v", err)
	}
	rig.disp.Resolver = &reg

	oc, err := Run(context.Background(), callLoadedDefinition(root, child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	calls := base.Calls()
	if len(calls) != 1 {
		t.Fatalf("base calls len = %d, want 1", len(calls))
	}
	if calls[0].Uses != childRef {
		t.Fatalf("AgentInvocation.Uses = %q, want child role ref %q", calls[0].Uses, childRef)
	}
	if got := calls[0].With["scope"]; got != "child" {
		t.Fatalf("AgentInvocation.With[scope] = %v, want child role config", got)
	}
}

func TestRunCallStepCallStartedPinsContainerlessMapBodyAgent(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	base := agentfake.New("test/base").WithVersion("base-v1").WithCaps(agent.Caps{Containerless: true})
	var reg agent.Registry
	if err := reg.Register(base); err != nil {
		t.Fatalf("Register base: %v", err)
	}

	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Graph: ir.NodeList{&ir.CallStep{ID: "recon", Call: "scan", Input: map[string]ir.TemplateValue{
			"items": json.RawMessage(`[]`),
		}}},
	}
	childInput := ir.JSONSchema{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{"type": "array"},
		},
		"required":             []any{"items"},
		"additionalProperties": false,
	}
	child := &ir.Workflow{
		ID:          "scan",
		Version:     1,
		InputSchema: &childInput,
		Agents: map[string]ir.AgentRole{
			"auditor": {Uses: "test/base"},
		},
		Graph: ir.NodeList{
			&ir.Map{
				Over:        ir.Expr("{{ input.items }}"),
				As:          "item",
				Container:   "lab",
				Concurrency: 1,
				Body: ir.NodeList{
					&ir.AgentStep{ID: "audit", Uses: "auditor"},
				},
			},
		},
	}
	childRef := AgentRuntimeRef(child, "mod-scan", "auditor")
	if err := reg.Register(agent.NewDerivedAdapter(childRef, base, child.Agents["auditor"].With)); err != nil {
		t.Fatalf("Register child role: %v", err)
	}
	rig.disp.Resolver = &reg

	oc, err := Run(context.Background(), callLoadedDefinition(root, child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	rec, ok := rig.rs.LookupCallStarted("recon")
	if !ok {
		t.Fatal("LookupCallStarted(recon) ok=false")
	}
	if len(rec.Runtimes) != 1 {
		t.Fatalf("call.started runtimes len = %d, want 1: %+v", len(rec.Runtimes), rec.Runtimes)
	}
	if rec.Runtimes[0].Ref != childRef || rec.Runtimes[0].Version != "base-v1" || rec.Runtimes[0].Container != "" {
		t.Fatalf("call.started runtime = %+v, want child role base-v1 containerless", rec.Runtimes[0])
	}
}

func TestRunPreflightsCompletedCallStartedRuntimeDrift(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Graph: ir.NodeList{&ir.CallStep{ID: "recon", Call: "scan"}},
	}
	child := &ir.Workflow{
		ID:      "scan",
		Version: 1,
		Agents: map[string]ir.AgentRole{
			"auditor": {Uses: "test/base"},
		},
		Containers: map[string]ir.Container{
			"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		},
		Graph: ir.NodeList{
			&ir.AgentStep{ID: "audit", Uses: "auditor", Container: "c"},
		},
	}
	childRef := AgentRuntimeRef(child, "mod-scan", "auditor")
	inputRef, err := rig.blobs.Put([]byte(`{}`))
	if err != nil {
		t.Fatalf("put input: %v", err)
	}
	if err := rig.log.Append(state.Event{
		Type: EventCallStarted,
		Path: "recon",
		Data: marshalOrFatal(t, CallStartedData{
			InputRef: inputRef,
			Runtimes: []ResolvedRuntime{{
				Ref:       childRef,
				Container: QualifiedContainerKey("recon.workflow", "c"),
				Version:   "base-v1",
			}},
		}),
	}); err != nil {
		t.Fatalf("seed call.started: %v", err)
	}
	if err := rig.log.Append(state.Event{
		Type: EventNodeCompleted,
		Path: "recon",
		Data: marshalOrFatal(t, NodeCompletedData{Outcome: string(OutcomeOK)}),
	}); err != nil {
		t.Fatalf("seed completed call: %v", err)
	}
	folded, err := Fold(mustFoldEvents(t, rig.log), rig.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	rig.rs = folded

	base := agentfake.New("test/base").WithVersion("base-v2")
	var reg agent.Registry
	if err := reg.Register(base); err != nil {
		t.Fatalf("Register base: %v", err)
	}
	if err := reg.Register(agent.NewDerivedAdapter(childRef, base, child.Agents["auditor"].With)); err != nil {
		t.Fatalf("Register child role: %v", err)
	}
	rig.disp.Resolver = &reg
	beforeEvents := len(callEvents(t, rig.log))

	oc, err := Run(context.Background(), callLoadedDefinition(root, child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty on runtime drift", oc)
	}
	var drift *ErrRuntimeDrift
	if !errors.As(err, &drift) {
		t.Fatalf("Run err = %v, want *ErrRuntimeDrift", err)
	}
	if drift.Ref != childRef || drift.Container != QualifiedContainerKey("recon.workflow", "c") || drift.Recorded != "base-v1" || drift.Current != "base-v2" {
		t.Fatalf("drift = %+v, want child role ref/container base-v1->base-v2", drift)
	}
	if got := len(callEvents(t, rig.log)); got != beforeEvents {
		t.Fatalf("log event count = %d, want unchanged %d", got, beforeEvents)
	}
}

func TestRunCallStepResumeRestoresChildSnapshotHandle(t *testing.T) {
	rig := newCallRunRig(t)
	rig.fake.WithBlobs(rig.blobs)
	rig.seedRunStarted(t)
	inputRef, err := rig.blobs.Put([]byte(`{}`))
	if err != nil {
		t.Fatalf("seed call input: %v", err)
	}
	if err := rig.log.Append(state.Event{
		Type: EventCallStarted,
		Path: "recon",
		Data: marshalOrFatal(t, CallStartedData{InputRef: inputRef}),
	}); err != nil {
		t.Fatalf("seed call.started: %v", err)
	}
	snapshotRef, err := rig.blobs.Put([]byte(`{}`))
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if err := rig.log.Append(state.Event{
		Type: EventNodeCompleted,
		Path: "recon.workflow.produce",
		Data: marshalOrFatal(t, NodeCompletedData{
			Outcome:     string(OutcomeOK),
			SnapshotRef: snapshotRef,
			Container:   QualifiedContainerKey("recon.workflow", "c"),
		}),
	}); err != nil {
		t.Fatalf("seed child node.completed: %v", err)
	}
	folded, err := Fold(mustFoldEvents(t, rig.log), rig.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	rig.rs = folded
	rig.fake.ProgramExec("consume", container.ExecResult{ExitCode: 0}, nil)
	child := &ir.Workflow{
		ID:      "scan",
		Version: 1,
		Containers: map[string]ir.Container{
			"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Snapshot: "workspace"},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "produce", Container: "c", Run: "produce"},
			&ir.CodeStep{ID: "consume", Container: "c", Run: "consume"},
		},
	}

	oc, err := Run(context.Background(), simpleCallNoInputDefinition(child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if len(rig.fake.RestoreCalls) != 1 {
		t.Fatalf("RestoreCalls len = %d, want 1", len(rig.fake.RestoreCalls))
	}
	restore := rig.fake.RestoreCalls[0]
	if restore.Name != "recon.workflow::c" || string(restore.Ref) != snapshotRef {
		t.Fatalf("RestoreCalls[0] = %+v, want name recon.workflow::c ref %q", restore, snapshotRef)
	}
	if len(rig.fake.ExecHandles) != 1 || rig.fake.ExecHandles[0].Name != "recon.workflow::c" {
		t.Fatalf("exec handles = %+v, want consume on restored qualified child handle", rig.fake.ExecHandles)
	}
}

func TestRunCallStepInputFilesAssetRefsUseChildModuleAssets(t *testing.T) {
	rig := newCallRunRig(t)
	rig.fake.WithBlobs(rig.blobs)
	rig.seedRunStarted(t)
	rootBytes := []byte("root schema\n")
	childBytes := []byte("child schema\n")
	rootRef, err := rig.blobs.Put(rootBytes)
	if err != nil {
		t.Fatalf("put root asset: %v", err)
	}
	childRef, err := rig.blobs.Put(childBytes)
	if err != nil {
		t.Fatalf("put child asset: %v", err)
	}
	rig.fake.ProgramExec("consume", container.ExecResult{ExitCode: 0}, nil)
	child := &ir.Workflow{
		ID:      "scan",
		Version: 1,
		Assets:  map[string]string{"schema": "child.schema.json"},
		Containers: map[string]ir.Container{
			"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Snapshot: "workspace"},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:         "consume",
				Container:  "c",
				Run:        "consume",
				InputFiles: map[string]string{"/work/schema.json": "asset.schema"},
			},
		},
	}
	def := simpleCallNoInputDefinition(child)
	def.Workflow.Assets = map[string]string{"schema": "root.schema.json"}

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{
		Assets: map[string]RunStartedAsset{
			"schema":          testRunStartedAsset("root.schema.json", rootRef, rootBytes),
			"mod-scan/schema": testRunStartedAsset("child.schema.json", childRef, childBytes),
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	var snapshotRef string
	for _, e := range callEvents(t, rig.log) {
		if e.Type != EventNodeCompleted || e.Path != "recon.workflow.consume" {
			continue
		}
		var d NodeCompletedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("unmarshal node.completed: %v", err)
		}
		snapshotRef = string(d.SnapshotRef)
	}
	if snapshotRef == "" {
		t.Fatalf("missing child snapshot ref in events")
	}
	h, err := rig.fake.Restore(context.Background(), container.SnapshotRef(snapshotRef), "inspect")
	if err != nil {
		t.Fatalf("Restore snapshot: %v", err)
	}
	got, err := rig.fake.CaptureFiles(context.Background(), h, []string{"/work/schema.json"})
	if err != nil {
		t.Fatalf("CaptureFiles: %v", err)
	}
	if string(got[0].Content) != string(childBytes) {
		t.Fatalf("staged asset bytes = %q, want child module bytes %q", got[0].Content, childBytes)
	}
}

func TestRunCallStepRecordsInputFilesBeforeChild(t *testing.T) {
	rig := newCallRunRig(t)
	spy := newCopyToSpy(rig.fake)
	rig.disp.Backend = spy
	rig.seedRunStarted(t)
	rootHandle, err := spy.Create(context.Background(), container.ContainerSpec{Name: "rootc"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	rig.disp.Handles["rootc"] = rootHandle
	reportBytes := []byte(`{"status":"ok"}`)
	rig.fake.ProgramExecWithFiles("collect", container.ExecResult{ExitCode: 0}, nil,
		map[string][]byte{"/out/report.json": reportBytes})
	rig.fake.ProgramExec("./use-report.sh", container.ExecResult{ExitCode: 0}, nil)
	def := callInputFileDefinition(
		map[string]string{"report": "step.collect.files.report"},
		ir.WorkflowInputFiles{"report": {Format: "json", Schema: awfStringObjectSchema("status")}},
	)

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	rec, ok := rig.rs.LookupCallStarted("analyze")
	if !ok {
		t.Fatal("LookupCallStarted(analyze) ok=false")
	}
	if rec.InputFiles["report"] == "" {
		t.Fatalf("call.started input_files.report = %q, want non-empty CAS ref", rec.InputFiles["report"])
	}
	if got := stagedFileContent(t, spy, "analyze.workflow::c", "/work/report.json"); string(got) != string(reportBytes) {
		t.Fatalf("staged report bytes = %q, want %q", got, reportBytes)
	}
	events := callEvents(t, rig.log)
	assertEventOrder(t, events, []eventAtPath{
		{typ: EventNodeStarted, path: "analyze"},
		{typ: EventCallStarted, path: "analyze"},
		{typ: EventNodeStarted, path: "analyze.workflow.use"},
	})
}

func TestRunCallStepInputFileContractFailureBeforeChildGraph(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rootHandle, err := rig.fake.Create(context.Background(), container.ContainerSpec{Name: "rootc"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	rig.disp.Handles["rootc"] = rootHandle
	rig.fake.ProgramExecWithFiles("collect", container.ExecResult{ExitCode: 0}, nil,
		map[string][]byte{"/out/report.json": []byte(`{"status":3}`)})
	rig.fake.ProgramExec("./use-report.sh", container.ExecResult{ExitCode: 0}, nil)
	def := callInputFileDefinition(
		map[string]string{"report": "step.collect.files.report"},
		ir.WorkflowInputFiles{"report": {Format: "json", Schema: awfStringObjectSchema("status")}},
	)

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if oc != OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (err=%v)", oc, OutcomePermanentFailure, err)
	}
	if err == nil || !strings.Contains(err.Error(), "input_files.report") {
		t.Fatalf("Run err = %v, want input_files.report contract error", err)
	}
	if sawExec(rig.fake, "./use-report.sh") {
		t.Fatalf("child exec ./use-report.sh ran despite call input file contract failure")
	}
	if _, ok := rig.rs.LookupCallStarted("analyze"); ok {
		t.Fatal("call.started recorded despite input file contract failure")
	}
	if sawCreateSpec(rig.fake, "analyze.workflow::c") {
		t.Fatalf("child runtime handle was created before input file validation failed")
	}
	if got := countEvents(callEvents(t, rig.log), EventNodeStarted, "analyze.workflow.use"); got != 0 {
		t.Fatalf("child node.started count = %d, want 0", got)
	}
}

func TestRunWorkflowInputFileRefWithoutCallerFailsBeforeDispatch(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rootHandle, err := rig.fake.Create(context.Background(), container.ContainerSpec{Name: "rootc"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	rig.disp.Handles["rootc"] = rootHandle
	rig.fake.ProgramExec("./use-report.sh", container.ExecResult{ExitCode: 0}, nil)
	wf := &ir.Workflow{
		ID:         "root",
		Version:    1,
		InputFiles: ir.WorkflowInputFiles{"report": {}},
		Containers: map[string]ir.Container{"rootc": {Image: "oci://root@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:         "use",
				Container:  "rootc",
				Run:        "./use-report.sh",
				InputFiles: map[string]string{"/work/report.json": "input.files.report"},
			},
		},
	}

	oc, err := Run(context.Background(), awfLoadedDefinitionForWorkflow(wf), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if oc != OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (err=%v)", oc, OutcomePermanentFailure, err)
	}
	if err == nil || !strings.Contains(err.Error(), "input.files.report") {
		t.Fatalf("Run err = %v, want missing caller workflow input file error", err)
	}
	if sawExec(rig.fake, "./use-report.sh") {
		t.Fatalf("dispatch ran despite missing caller input file")
	}
}

func TestRunCallStepRecordedInputFileRefWinsOverParentArtifact(t *testing.T) {
	rig := newCallRunRig(t)
	spy := newCopyToSpy(rig.fake)
	rig.disp.Backend = spy
	rig.seedRunStarted(t)
	currentRef, err := rig.blobs.Put([]byte(`{"status":"current"}`))
	if err != nil {
		t.Fatalf("put current report: %v", err)
	}
	recordedBytes := []byte(`{"status":"recorded"}`)
	recordedRef, err := rig.blobs.Put(recordedBytes)
	if err != nil {
		t.Fatalf("put recorded report: %v", err)
	}
	inputRef, err := rig.blobs.Put([]byte(`{}`))
	if err != nil {
		t.Fatalf("put input: %v", err)
	}
	if err := rig.log.Append(state.Event{
		Type: EventNodeCompleted,
		Path: "collect",
		Data: marshalOrFatal(t, NodeCompletedData{
			Outcome: string(OutcomeOK),
			Files:   map[string]string{"report": currentRef},
		}),
	}); err != nil {
		t.Fatalf("seed collect completion: %v", err)
	}
	if err := rig.log.Append(state.Event{
		Type: EventCallStarted,
		Path: "analyze",
		Data: marshalOrFatal(t, CallStartedData{
			InputRef:   inputRef,
			InputFiles: map[string]string{"report": recordedRef},
		}),
	}); err != nil {
		t.Fatalf("seed call.started: %v", err)
	}
	folded, err := Fold(mustFoldEvents(t, rig.log), rig.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	rig.rs = folded
	rig.fake.ProgramExec("./use-report.sh", container.ExecResult{ExitCode: 0}, nil)
	def := callInputFileDefinition(
		map[string]string{"report": "step.collect.files.report"},
		ir.WorkflowInputFiles{"report": {Format: "json", Schema: awfStringObjectSchema("status")}},
	)

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if got := stagedFileContent(t, spy, "analyze.workflow::c", "/work/report.json"); string(got) != string(recordedBytes) {
		t.Fatalf("staged report bytes = %q, want recorded bytes %q", got, recordedBytes)
	}
}

func TestRunCallStepInputFileSchemaRefUsesChildModuleAssets(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rootHandle, err := rig.fake.Create(context.Background(), container.ContainerSpec{Name: "rootc"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	rig.disp.Handles["rootc"] = rootHandle
	rootSchema := []byte(`{"type":"object","required":["status"],"properties":{"status":{"const":"root"}}}`)
	childSchema := []byte(`{"type":"object","required":["status"],"properties":{"status":{"const":"child"}}}`)
	rootSchemaRef, err := rig.blobs.Put(rootSchema)
	if err != nil {
		t.Fatalf("put root schema: %v", err)
	}
	childSchemaRef, err := rig.blobs.Put(childSchema)
	if err != nil {
		t.Fatalf("put child schema: %v", err)
	}
	rig.fake.ProgramExecWithFiles("collect", container.ExecResult{ExitCode: 0}, nil,
		map[string][]byte{"/out/report.json": []byte(`{"status":"child"}`)})
	rig.fake.ProgramExec("./use-report.sh", container.ExecResult{ExitCode: 0}, nil)
	def := callInputFileDefinition(
		map[string]string{"report": "step.collect.files.report"},
		ir.WorkflowInputFiles{"report": {Format: "json", SchemaRef: "asset.schema"}},
	)
	def.Workflow.Assets = map[string]string{"schema": "root.schema.json"}
	def.Modules["mod-scan"].Workflow.Assets = map[string]string{"schema": "child.schema.json"}

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{
		Assets: map[string]RunStartedAsset{
			"schema":          testRunStartedAsset("root.schema.json", rootSchemaRef, rootSchema),
			"mod-scan/schema": testRunStartedAsset("child.schema.json", childSchemaRef, childSchema),
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
}

func TestRunCallStepInputFileFailureIsCatchableByTry(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rootHandle, err := rig.fake.Create(context.Background(), container.ContainerSpec{Name: "rootc"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	rig.disp.Handles["rootc"] = rootHandle
	rig.fake.ProgramExecWithFiles("collect", container.ExecResult{ExitCode: 0}, nil,
		map[string][]byte{"/out/report.json": []byte(`{"status":3}`)})
	rig.fake.ProgramExec("./use-report.sh", container.ExecResult{ExitCode: 0}, nil)
	rig.fake.ProgramExec("./recover.sh", container.ExecResult{ExitCode: 0}, nil)
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Containers: map[string]ir.Container{"rootc": {Image: "oci://root@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:          "collect",
				Container:   "rootc",
				Run:         "collect",
				OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/report.json"}},
			},
			&ir.Try{
				Do: ir.NodeList{
					&ir.CallStep{
						ID:         "analyze",
						Call:       "scan",
						InputFiles: map[string]string{"report": "step.collect.files.report"},
					},
				},
				Catch: ir.NodeList{
					&ir.CodeStep{ID: "recover", Container: "rootc", Run: "./recover.sh"},
				},
			},
		},
	}
	child := childWorkflowUsingReport(ir.WorkflowInputFiles{"report": {Format: "json", Schema: awfStringObjectSchema("status")}})

	oc, err := Run(context.Background(), callLoadedDefinition(root, child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if !sawExec(rig.fake, "./recover.sh") {
		t.Fatalf("recover exec was not observed after catchable call input file failure")
	}
	if sawExec(rig.fake, "./use-report.sh") {
		t.Fatalf("child exec ./use-report.sh ran despite input file contract failure")
	}
}

func TestRunCallStepRecordedInputFileMissingBlobIsInternalError(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	inputRef, err := rig.blobs.Put([]byte(`{}`))
	if err != nil {
		t.Fatalf("put input: %v", err)
	}
	currentRef, err := rig.blobs.Put([]byte(`{"status":"current"}`))
	if err != nil {
		t.Fatalf("put current report: %v", err)
	}
	if err := rig.log.Append(state.Event{
		Type: EventNodeCompleted,
		Path: "collect",
		Data: marshalOrFatal(t, NodeCompletedData{
			Outcome: string(OutcomeOK),
			Files:   map[string]string{"report": currentRef},
		}),
	}); err != nil {
		t.Fatalf("seed collect completion: %v", err)
	}
	if err := rig.log.Append(state.Event{
		Type: EventCallStarted,
		Path: "analyze",
		Data: marshalOrFatal(t, CallStartedData{
			InputRef:   inputRef,
			InputFiles: map[string]string{"report": "sha256:missing-report"},
		}),
	}); err != nil {
		t.Fatalf("seed call.started: %v", err)
	}
	folded, err := Fold(mustFoldEvents(t, rig.log), rig.blobs)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	rig.rs = folded
	rig.fake.ProgramExec("./use-report.sh", container.ExecResult{ExitCode: 0}, nil)
	def := callInputFileDefinition(
		map[string]string{"report": "step.collect.files.report"},
		ir.WorkflowInputFiles{"report": {Format: "json", Schema: awfStringObjectSchema("status")}},
	)

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if oc != "" {
		t.Fatalf("Outcome = %q, want empty internal-error outcome", oc)
	}
	if err == nil || !strings.Contains(err.Error(), "input_files artifact fetch failed") {
		t.Fatalf("Run err = %v, want input_files artifact fetch failed", err)
	}
	if got := countEvents(callEvents(t, rig.log), EventNodeFailed, "analyze"); got != 0 {
		t.Fatalf("node.failed analyze count = %d, want 0 for internal fetch error", got)
	}
}

func TestRunCallStepOutputFilesSchemaRefUsesChildModuleAssets(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rootSchema := []byte(`{"type":"object","required":["kind"],"properties":{"kind":{"const":"root"}}}`)
	childSchema := []byte(`{"type":"object","required":["kind"],"properties":{"kind":{"const":"child"}}}`)
	rootRef, err := rig.blobs.Put(rootSchema)
	if err != nil {
		t.Fatalf("put root schema: %v", err)
	}
	childRef, err := rig.blobs.Put(childSchema)
	if err != nil {
		t.Fatalf("put child schema: %v", err)
	}
	rig.fake.ProgramExecWithFiles("produce", container.ExecResult{ExitCode: 0}, nil,
		map[string][]byte{"/out/result.json": []byte(`{"kind":"child"}`)})
	child := &ir.Workflow{
		ID:      "scan",
		Version: 1,
		Assets:  map[string]string{"schema": "child.schema.json"},
		Containers: map[string]ir.Container{
			"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:        "produce",
				Container: "c",
				Run:       "produce",
				OutputFiles: ir.OutputFiles{{
					Name:      "result",
					Path:      "/out/result.json",
					Format:    "json",
					SchemaRef: "asset.schema",
				}},
			},
		},
	}
	def := simpleCallNoInputDefinition(child)
	def.Workflow.Assets = map[string]string{"schema": "root.schema.json"}

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{
		Assets: map[string]RunStartedAsset{
			"schema":          testRunStartedAsset("root.schema.json", rootRef, rootSchema),
			"mod-scan/schema": testRunStartedAsset("child.schema.json", childRef, childSchema),
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if _, ok := rig.rs.Completed["recon.workflow.produce"]; !ok {
		t.Fatal("missing child produce completion")
	}
}

func TestRunCallStepChildMapReduceReusesQualifiedContainer(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rig.fake.ProgramExec("scan a", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"k":"a"}`),
	}, nil)
	rig.fake.ProgramExec("merge", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"csv_rows":1}`),
	}, nil)
	child := &ir.Workflow{
		ID:      "scan",
		Version: 1,
		InputSchema: &ir.JSONSchema{
			"type":       "object",
			"properties": map[string]any{"items": map[string]any{"type": "array"}},
		},
		Containers: map[string]ir.Container{
			"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		},
		Graph: ir.NodeList{
			&ir.Map{
				ID:          "fanout",
				Over:        ir.Expr("{{ input.items }}"),
				As:          "item",
				Container:   "c",
				Concurrency: 1,
				Body: ir.NodeList{
					&ir.CodeStep{ID: "scan", Container: "c", Run: "scan {{ item }}", OutputSchema: awfStringObjectSchema("k")},
				},
				Reduce: &ir.Reduce{Run: "merge", Container: "c", OutputSchema: awfIntegerObjectSchema("csv_rows")},
			},
		},
	}
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Graph: ir.NodeList{
			&ir.CallStep{ID: "recon", Call: "scan", Input: map[string]ir.TemplateValue{
				"items": json.RawMessage(`["a"]`),
			}},
		},
	}

	oc, err := Run(context.Background(), callLoadedDefinition(root, child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	var childContainerCreates int
	for _, spec := range rig.fake.CreateSpecs {
		if spec.Name == "recon.workflow::c" {
			childContainerCreates++
		}
	}
	if childContainerCreates != 1 {
		t.Fatalf("qualified child container creates = %d, want 1 (reducer must reuse the declared handle)", childContainerCreates)
	}
	var reduceHandle *container.Handle
	for i, call := range rig.fake.Calls {
		if call.Run == "merge" {
			reduceHandle = &rig.fake.ExecHandles[i]
			break
		}
	}
	if reduceHandle == nil {
		t.Fatal("reduce command was not executed")
	}
	if reduceHandle.Name != "recon.workflow::c" {
		t.Fatalf("reduce handle name = %q, want recon.workflow::c", reduceHandle.Name)
	}
}

// A run: reducer inside a CALLED sub-workflow must resolve its template refs
// against the SAME (child) scope the rest of the sub-workflow uses — the call
// frame's prefix-stripped RunState + typed-call input. Before the fix the reducer
// built its scope off the PARENT RunState + the PREFIXED map path with no input
// override, so an OUTER sibling step ref (step.resolve_digests, committed at
// recon.workflow.resolve_digests) was looked up at the bare key "resolve_digests"
// and missed -> AWF4002 "step ... not yet committed". The map's own `over` used
// the same ref and resolved fine (it goes through ictx.scope), which is what made
// this call-specific. Regression for the cve-feasibility version-universe report.
func TestReduceInCalledSubworkflowResolvesOuterStepRefAndInput(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	// Child outer sibling step (committed at recon.workflow.resolve_digests).
	rig.fake.ProgramExec("resolve-digests", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"digest":"sha256:deadbeef"}`),
	}, nil)
	// Map body, one item.
	rig.fake.ProgramExec("scan a", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"k":"a"}`),
	}, nil)
	// The reducer command AFTER templating against the OUTER sibling step output
	// AND the typed call input. If resolution is wrong this exact command never
	// runs and the run fails.
	rig.fake.ProgramExec("merge sha256:deadbeef example.com", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"csv_rows":1}`),
	}, nil)
	child := &ir.Workflow{
		ID:      "scan",
		Version: 1,
		InputSchema: &ir.JSONSchema{
			"type": "object",
			"properties": map[string]any{
				"items":  map[string]any{"type": "array"},
				"target": map[string]any{"type": "string"},
			},
		},
		Containers: map[string]ir.Container{
			"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "resolve_digests", Container: "c", Run: "resolve-digests", OutputSchema: awfStringObjectSchema("digest")},
			&ir.Map{
				ID:          "fanout",
				Over:        ir.Expr("{{ input.items }}"),
				As:          "item",
				Container:   "c",
				Concurrency: 1,
				Body: ir.NodeList{
					&ir.CodeStep{ID: "scan", Container: "c", Run: "scan {{ item }}", OutputSchema: awfStringObjectSchema("k")},
				},
				Reduce: &ir.Reduce{
					Run:          "merge {{ step.resolve_digests.digest }} {{ input.target }}",
					Container:    "c",
					OutputSchema: awfIntegerObjectSchema("csv_rows"),
				},
			},
		},
	}
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{"scan": "scan.awf.yaml"},
		Graph: ir.NodeList{
			&ir.CallStep{ID: "recon", Call: "scan", Input: map[string]ir.TemplateValue{
				"items":  json.RawMessage(`["a"]`),
				"target": json.RawMessage(`"example.com"`),
			}},
		},
	}

	oc, err := Run(context.Background(), callLoadedDefinition(root, child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if !sawReduceCmd(rig.fake, "merge sha256:deadbeef example.com") {
		t.Fatalf("reducer command not templated against outer step + call input; Calls=%v", reduceCmdRuns(rig.fake))
	}
}

// A run: reducer inside a CALLED sub-workflow must also resolve BODY-step
// aggregate refs (step.<bodyId>.<field> -> the index-ordered canonical-JSON array
// of committed per-item outputs). Before the fix the reducer's mapPath stayed
// PREFIXED (recon.workflow.map[0]) while ir.SingleMapBodyShape yields the static
// "map[0]", so body-step aggregate detection mismatched and fell through to a
// parent-rs lookup that produced an empty/unresolvable aggregate. Stripping the
// call prefix from BOTH the base scope (child rs) and the mapPath fixes it.
func TestReduceInCalledSubworkflowResolvesBodyAggregateRef(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rig.fake.ProgramExec("scan a", container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"k":"a"}`)}, nil)
	// {{ step.scan.k }} aggregates the one item's k into the canonical JSON ["a"].
	rig.fake.ProgramExec(`merge-agg ["a"]`, container.ExecResult{ExitCode: 0, AWFOutput: []byte(`{"csv_rows":1}`)}, nil)
	child := &ir.Workflow{
		ID:      "scan",
		Version: 1,
		InputSchema: &ir.JSONSchema{
			"type":       "object",
			"properties": map[string]any{"items": map[string]any{"type": "array"}},
		},
		Containers: map[string]ir.Container{
			"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		},
		Graph: ir.NodeList{
			&ir.Map{
				ID:          "fanout",
				Over:        ir.Expr("{{ input.items }}"),
				As:          "item",
				Container:   "c",
				Concurrency: 1,
				Body: ir.NodeList{
					&ir.CodeStep{ID: "scan", Container: "c", Run: "scan {{ item }}", OutputSchema: awfStringObjectSchema("k")},
				},
				Reduce: &ir.Reduce{
					Run:          `merge-agg {{ step.scan.k }}`,
					Container:    "c",
					OutputSchema: awfIntegerObjectSchema("csv_rows"),
				},
			},
		},
	}
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{"scan": "scan.awf.yaml"},
		Graph: ir.NodeList{
			&ir.CallStep{ID: "recon", Call: "scan", Input: map[string]ir.TemplateValue{
				"items": json.RawMessage(`["a"]`),
			}},
		},
	}

	oc, err := Run(context.Background(), callLoadedDefinition(root, child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if !sawReduceCmd(rig.fake, `merge-agg ["a"]`) {
		t.Fatalf("reducer body-aggregate ref not rendered to canonical JSON; Calls=%v", reduceCmdRuns(rig.fake))
	}
}

func sawReduceCmd(f *container.Fake, want string) bool {
	for _, c := range f.Calls {
		if c.Run == want {
			return true
		}
	}
	return false
}

func reduceCmdRuns(f *container.Fake) []string {
	runs := make([]string, 0, len(f.Calls))
	for _, c := range f.Calls {
		runs = append(runs, c.Run)
	}
	return runs
}

func TestRunCallStepChildSignalWhereUsesChildScope(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	b := tempBroker(t)
	if _, err := b.WriteSignal("ready", []byte(`{"token":"parent-token","value":"wrong"}`)); err != nil {
		t.Fatalf("WriteSignal wrong: %v", err)
	}
	if _, err := b.WriteSignal("ready", []byte(`{"token":"child-token","value":"child-value"}`)); err != nil {
		t.Fatalf("WriteSignal match: %v", err)
	}
	rig.fake.ProgramExec("produce", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"value":"child-value"}`),
	}, nil)
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Graph: ir.NodeList{
			&ir.CallStep{ID: "recon", Call: "scan", Input: map[string]ir.TemplateValue{
				"token": json.RawMessage(`"child-token"`),
			}},
		},
	}
	child := &ir.Workflow{
		ID:          "scan",
		Version:     1,
		InputSchema: awfStringObjectSchema("token"),
		Containers:  map[string]ir.Container{"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "produce", Container: "c", Run: "produce", OutputSchema: awfStringObjectSchema("value")},
			&ir.SignalStep{
				ID:           "wait",
				Await:        "ready",
				Where:        `token == "{{ input.token }}" && value == "{{ step.produce.value }}"`,
				OutputSchema: signalTokenValueSchema(),
			},
		},
	}

	oc, err := Run(context.Background(), callLoadedDefinition(root, child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{Broker: b})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	nr, ok := rig.rs.LookupCompleted("recon.workflow.wait")
	if !ok {
		t.Fatal("missing child signal completion")
	}
	if nr.Outputs["token"] != "child-token" || nr.Outputs["value"] != "child-value" {
		t.Fatalf("signal outputs = %+v, want child token/value", nr.Outputs)
	}
	d, err := b.Receive(context.Background(), "ready", 0)
	if err != nil {
		t.Fatalf("Receive remaining: %v", err)
	}
	if d.Seq != 1 {
		t.Fatalf("remaining signal seq = %d, want non-matching seq 1 left buffered", d.Seq)
	}
}

func TestRunCallStepRepeatedCallsHaveIsolatedContainers(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rig.fake.ProgramExec("write first", container.ExecResult{ExitCode: 0}, nil)
	rig.fake.ProgramExec("write second", container.ExecResult{ExitCode: 0}, nil)
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Graph: ir.NodeList{
			&ir.CallStep{ID: "first", Call: "scan", Input: map[string]ir.TemplateValue{
				"label": json.RawMessage(`"first"`),
			}},
			&ir.CallStep{ID: "second", Call: "scan", Input: map[string]ir.TemplateValue{
				"label": json.RawMessage(`"second"`),
			}},
		},
	}

	oc, err := Run(context.Background(), callLoadedDefinition(root, childNoOutputWorkflow("scan", "write {{ input.label }}")), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if len(rig.fake.ExecHandles) != 2 {
		t.Fatalf("ExecHandles len = %d, want 2", len(rig.fake.ExecHandles))
	}
	if rig.fake.ExecHandles[0].ID == rig.fake.ExecHandles[1].ID {
		t.Fatalf("repeated calls reused handle ID %q; want isolated containers", rig.fake.ExecHandles[0].ID)
	}
	if rig.fake.ExecHandles[0].Name != "first.workflow::c" || rig.fake.ExecHandles[1].Name != "second.workflow::c" {
		t.Fatalf("handle names = (%q,%q), want qualified call containers", rig.fake.ExecHandles[0].Name, rig.fake.ExecHandles[1].Name)
	}
}

func TestRunCallStepNestedCallPaths(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rig.fake.ProgramExec("inner", container.ExecResult{ExitCode: 0}, nil)
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"outer": "outer.awf.yaml",
		},
		Graph: ir.NodeList{&ir.CallStep{ID: "outer", Call: "outer"}},
	}
	outer := &ir.Workflow{
		ID:      "outer",
		Version: 1,
		Imports: map[string]string{
			"inner": "inner.awf.yaml",
		},
		Graph: ir.NodeList{&ir.CallStep{ID: "inner", Call: "inner"}},
	}
	inner := childNoOutputWorkflow("inner", "inner")
	inner.InputSchema = nil
	def := callLoadedDefinition(root, outer)
	def.ImportEdges[0] = ir.LoadedImportEdge{ParentID: "", ImportID: "outer", ChildID: "mod-scan"}
	def.Modules["mod-inner"] = &ir.LoadedModule{ID: "mod-inner", Workflow: inner, ComposeFiles: map[string][]byte{}}
	def.ImportEdges = append(def.ImportEdges, ir.LoadedImportEdge{ParentID: "mod-scan", ImportID: "inner", ChildID: "mod-inner"})

	oc, err := Run(context.Background(), def, rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc != OutcomeOK {
		t.Fatalf("Outcome = %q, want %q", oc, OutcomeOK)
	}
	if _, ok := rig.rs.Completed["outer.workflow.inner.workflow.work"]; !ok {
		t.Fatalf("missing nested child completion outer.workflow.inner.workflow.work")
	}
}

func TestRunCallStepChildScopeHidesParentSteps(t *testing.T) {
	rig := newCallRunRig(t)
	rig.seedRunStarted(t)
	rig.fake.ProgramExec("parent", container.ExecResult{
		ExitCode:  0,
		AWFOutput: []byte(`{"summary":"parent-only"}`),
	}, nil)
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Containers: map[string]ir.Container{"rootc": {Image: "oci://root@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "parent", Container: "rootc", Run: "parent", OutputSchema: awfStringObjectSchema("summary")},
			&ir.CallStep{ID: "recon", Call: "scan"},
		},
	}
	rootHandle, err := rig.fake.Create(context.Background(), container.ContainerSpec{Name: "rootc"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	rig.disp.Handles["rootc"] = rootHandle
	child := childNoOutputWorkflow("scan", "child sees {{ step.parent.summary }}")
	child.InputSchema = nil

	oc, err := Run(context.Background(), callLoadedDefinition(root, child), rig.rs, rig.disp, rig.log, rig.blobs, rig.clk, RunOptions{})
	if oc != OutcomePermanentFailure {
		t.Fatalf("Outcome = %q, want %q (err=%v)", oc, OutcomePermanentFailure, err)
	}
	if err == nil || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("err = %v, want unresolved parent step ref", err)
	}
}

type eventAtPath struct {
	typ  string
	path string
}

func simpleCallDefinition(child *ir.Workflow) *ir.LoadedDefinition {
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Graph: ir.NodeList{
			&ir.CallStep{ID: "recon", Call: "scan", Input: map[string]ir.TemplateValue{
				"topic": json.RawMessage(`"child"`),
			}},
		},
	}
	return callLoadedDefinition(root, child)
}

func simpleCallNoInputDefinition(child *ir.Workflow) *ir.LoadedDefinition {
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Graph: ir.NodeList{&ir.CallStep{ID: "recon", Call: "scan"}},
	}
	return callLoadedDefinition(root, child)
}

func callLoadedDefinition(root, child *ir.Workflow) *ir.LoadedDefinition {
	return &ir.LoadedDefinition{
		Workflow:     root,
		WorkflowPath: "/repo/root.awf.yaml",
		ComposeFiles: map[string][]byte{},
		Modules: map[string]*ir.LoadedModule{
			"": {
				ID:           "",
				Workflow:     root,
				WorkflowPath: "/repo/root.awf.yaml",
				ComposeFiles: map[string][]byte{},
			},
			"mod-scan": {
				ID:           "mod-scan",
				Workflow:     child,
				WorkflowPath: "/repo/scan.awf.yaml",
				ComposeFiles: map[string][]byte{},
			},
		},
		ImportEdges: []ir.LoadedImportEdge{{ParentID: "", ImportID: "scan", ChildID: "mod-scan"}},
	}
}

func callInputFileDefinition(callInputFiles map[string]string, childInputFiles ir.WorkflowInputFiles) *ir.LoadedDefinition {
	root := &ir.Workflow{
		ID:      "root",
		Version: 1,
		Imports: map[string]string{
			"scan": "scan.awf.yaml",
		},
		Containers: map[string]ir.Container{"rootc": {Image: "oci://root@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:          "collect",
				Container:   "rootc",
				Run:         "collect",
				OutputFiles: ir.OutputFiles{{Name: "report", Path: "/out/report.json"}},
			},
			&ir.CallStep{
				ID:         "analyze",
				Call:       "scan",
				InputFiles: callInputFiles,
			},
		},
	}
	return callLoadedDefinition(root, childWorkflowUsingReport(childInputFiles))
}

func childWorkflowUsingReport(inputFiles ir.WorkflowInputFiles) *ir.Workflow {
	return &ir.Workflow{
		ID:         "scan",
		Version:    1,
		InputFiles: inputFiles,
		Containers: map[string]ir.Container{"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:         "use",
				Container:  "c",
				Run:        "./use-report.sh",
				InputFiles: map[string]string{"/work/report.json": "input.files.report"},
			},
		},
	}
}

type failSecondCreateBackend struct {
	*container.Fake
	creates int
}

func (b *failSecondCreateBackend) Create(ctx context.Context, spec container.ContainerSpec) (container.Handle, error) {
	b.creates++
	if b.creates == 2 {
		b.CreateSpecs = append(b.CreateSpecs, spec)
		return container.Handle{}, assertCreateFailure{spec: spec.Name}
	}
	return b.Fake.Create(ctx, spec)
}

type assertCreateFailure struct {
	spec string
}

func (e assertCreateFailure) Error() string {
	return "programmed create failure for " + e.spec
}

func childSummaryWorkflow(id, run string) *ir.Workflow {
	return &ir.Workflow{
		ID:           id,
		Version:      1,
		InputSchema:  awfStringObjectSchema("topic"),
		Containers:   map[string]ir.Container{"c": {Image: "oci://child@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
		OutputSchema: awfStringObjectSchema("summary"),
		Outputs: map[string]ir.TemplateValue{
			"summary": json.RawMessage(`"{{ step.summarize.summary }}"`),
		},
		Graph: ir.NodeList{
			&ir.CodeStep{
				ID:           "summarize",
				Container:    "c",
				Run:          run,
				OutputSchema: awfStringObjectSchema("summary"),
			},
		},
	}
}

func childNoOutputWorkflow(id, run string) *ir.Workflow {
	return &ir.Workflow{
		ID:          id,
		Version:     1,
		InputSchema: awfStringObjectSchema("label"),
		Containers:  map[string]ir.Container{"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "work", Container: "c", Run: run},
		},
	}
}

func testRunStartedAsset(declaredPath, ref string, b []byte) RunStartedAsset {
	sum := sha256.Sum256(b)
	return RunStartedAsset{
		DeclaredPath: declaredPath,
		Files: []RunStartedAssetFile{{
			Path:   ".",
			Ref:    ref,
			Size:   int64(len(b)),
			SHA256: hex.EncodeToString(sum[:]),
		}},
	}
}

func awfIntegerObjectSchema(fields ...string) *ir.JSONSchema {
	props := map[string]any{}
	required := make([]any, 0, len(fields))
	for _, field := range fields {
		props[field] = map[string]any{"type": "integer"}
		required = append(required, field)
	}
	schema := ir.JSONSchema{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
	return &schema
}

func signalTokenValueSchema() *ir.JSONSchema {
	return &ir.JSONSchema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"token", "value"},
		"properties": map[string]any{
			"token": map[string]any{"type": "string"},
			"value": map[string]any{"type": "string"},
		},
	}
}

func callEvents(t *testing.T, log *state.InMemoryLog) []state.Event {
	t.Helper()
	return mustFoldEvents(t, log)
}

func mustFoldEvents(t *testing.T, log *state.InMemoryLog) []state.Event {
	t.Helper()
	events, err := log.Fold()
	if err != nil {
		t.Fatalf("log.Fold: %v", err)
	}
	return events
}

func assertEventOrder(t *testing.T, events []state.Event, want []eventAtPath) {
	t.Helper()
	pos := 0
	for _, e := range events {
		if pos >= len(want) {
			return
		}
		if e.Type == want[pos].typ && e.Path == want[pos].path {
			pos++
		}
	}
	if pos != len(want) {
		t.Fatalf("event order matched %d/%d; events=%+v want=%+v", pos, len(want), events, want)
	}
}

func countEvents(events []state.Event, typ, path string) int {
	n := 0
	for _, e := range events {
		if e.Type == typ && e.Path == path {
			n++
		}
	}
	return n
}

func sawExec(fake *container.Fake, run string) bool {
	for _, call := range fake.Calls {
		if call.Run == run {
			return true
		}
	}
	return false
}

func sawCreateSpec(fake *container.Fake, name string) bool {
	for _, spec := range fake.CreateSpecs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

func stagedFileContent(t *testing.T, spy *copyToSpy, handleName, filePath string) []byte {
	t.Helper()
	var handleID string
	for _, h := range spy.ExecHandles {
		if h.Name == handleName {
			handleID = h.ID
			break
		}
	}
	if handleID == "" {
		t.Fatalf("no exec handle recorded for %q; ExecHandles=%+v", handleName, spy.ExecHandles)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	for _, f := range spy.staged[handleID] {
		if f.Path == filePath {
			out := make([]byte, len(f.Content))
			copy(out, f.Content)
			return out
		}
	}
	t.Fatalf("no staged file %q for handle %q (%s); staged=%+v", filePath, handleName, handleID, spy.staged[handleID])
	return nil
}

func eventIndex(events []state.Event, typ, path string) int {
	for i, e := range events {
		if e.Type == typ && e.Path == path {
			return i
		}
	}
	return -1
}
