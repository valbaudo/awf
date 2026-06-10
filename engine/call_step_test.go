package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	inner.Input = nil
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
	child.Input = nil

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

func childSummaryWorkflow(id, run string) *ir.Workflow {
	return &ir.Workflow{
		ID:           id,
		Version:      1,
		Input:        awfStringObjectSchema("topic"),
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
		ID:         id,
		Version:    1,
		Input:      awfStringObjectSchema("label"),
		Containers: map[string]ir.Container{"c": {Image: "oci://child@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}},
		Graph: ir.NodeList{
			&ir.CodeStep{ID: "work", Container: "c", Run: run},
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

func eventIndex(events []state.Event, typ, path string) int {
	for i, e := range events {
		if e.Type == typ && e.Path == path {
			return i
		}
	}
	return -1
}
