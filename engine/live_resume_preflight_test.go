package engine_test

import (
	"testing"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/fake"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/ir"
)

func TestLiveResumePreflightBuildsPersistentFrontierRequest(t *testing.T) {
	t.Parallel()

	live := fake.New("live/agent").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.CodeStep{ID: "first", Run: "already committed"},
		&ir.AgentStep{
			ID:   "next",
			Uses: "live/agent",
			With: ir.RawConfig{
				"prompt":  "continue from {{ step.first.value }}",
				"session": "opaque-session",
			},
		},
	}}}
	rs := engine.NewRunState("run-live", "digest", nil)
	rs.Epoch = 4
	rs.RecordCompleted("first", engine.NodeResult{
		Outcome: engine.OutcomeOK,
		Outputs: map[string]any{"value": "committed"},
	})

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("requests len = %d, want 1: %+v", len(reqs), reqs)
	}
	req := reqs[0]
	if req.NodePath != "next" {
		t.Errorf("NodePath = %q, want next", req.NodePath)
	}
	if req.AdapterRef != "live/agent" {
		t.Errorf("AdapterRef = %q, want live/agent", req.AdapterRef)
	}
	if got := req.With["prompt"]; got != "continue from committed" {
		t.Errorf("With[prompt] = %#v, want resolved prompt", got)
	}
	if got := req.With["session"]; got != "opaque-session" {
		t.Errorf("With[session] = %#v, want opaque pass-through", got)
	}
	if req.RunID != "run-live" || req.CurrentEpoch != 4 || req.NextEpoch != 5 {
		t.Errorf("run context = (%q,%d,%d), want (run-live,4,5)", req.RunID, req.CurrentEpoch, req.NextEpoch)
	}
}

func TestLiveResumePreflightDoesNotInspectWithKeys(t *testing.T) {
	t.Parallel()

	live := fake.New("live/opaque").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.AgentStep{
			ID:   "live",
			Uses: "live/opaque",
			With: ir.RawConfig{
				"cwd":            12345,
				"session":        map[string]any{"provider": "owns-this"},
				"provider_field": "literal",
			},
		},
	}}}
	rs := engine.NewRunState("run-opaque", "digest", nil)

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("requests len = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if got := req.With["cwd"]; got != 12345 {
		t.Errorf("With[cwd] = %#v, want untouched non-string", got)
	}
	session, ok := req.With["session"].(map[string]any)
	if !ok || session["provider"] != "owns-this" {
		t.Fatalf("With[session] = %#v, want opaque nested map", req.With["session"])
	}
	if got := req.With["provider_field"]; got != "literal" {
		t.Errorf("With[provider_field] = %#v, want pass-through", got)
	}
}

func TestLiveResumePreflightCollectsParallelFrontier(t *testing.T) {
	t.Parallel()

	live := fake.New("live/agent").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.Parallel{Children: ir.NodeList{
			&ir.AgentStep{ID: "left", Uses: "live/agent", With: ir.RawConfig{"side": "left"}},
			&ir.AgentStep{ID: "right", Uses: "live/agent", With: ir.RawConfig{"side": "right"}},
		}},
	}}}
	rs := engine.NewRunState("run-parallel", "digest", nil)

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("requests len = %d, want 2: %+v", len(reqs), reqs)
	}
	got := map[string]string{}
	for _, req := range reqs {
		got[req.NodePath] = req.With["side"].(string)
	}
	if got["parallel[0].left"] != "left" || got["parallel[0].right"] != "right" {
		t.Fatalf("parallel requests = %#v, want left/right runtime paths", got)
	}
}

func TestLiveResumePreflightRecomputesUnrecordedIfBranch(t *testing.T) {
	t.Parallel()

	live := fake.New("live/agent").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.If{
			Cond: "{{ true }}",
			Then: ir.NodeList{&ir.AgentStep{ID: "live", Uses: "live/agent"}},
			Else: ir.NodeList{&ir.AgentStep{ID: "other", Uses: "live/agent"}},
		},
	}}}
	rs := engine.NewRunState("run-if", "digest", nil)

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].NodePath != "if[0].then.live" {
		t.Fatalf("requests = %+v, want one then-branch live request", reqs)
	}
}

func TestLiveResumePreflightLoopUntilFirstIteration(t *testing.T) {
	t.Parallel()

	live := fake.New("live/agent").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	until := ir.Expr("{{ step.live.done }}")
	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			Until: &until,
			Body:  ir.NodeList{&ir.AgentStep{ID: "live", Uses: "live/agent"}},
		},
	}}}
	rs := engine.NewRunState("run-loop", "digest", nil)

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].NodePath != "loop[0].body.iter-1.live" {
		t.Fatalf("requests = %+v, want first loop iteration live request", reqs)
	}
}

func TestLiveResumePreflightLoopUntilExitContinuesToFollowingLive(t *testing.T) {
	t.Parallel()

	live := fake.New("live/agent").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	until := ir.Expr("{{ step.check.done }}")
	maxIters := 5
	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.Loop{
			Until:    &until,
			MaxIters: &maxIters,
			Body: ir.NodeList{&ir.CodeStep{
				ID: "check",
			}},
		},
		&ir.AgentStep{ID: "after", Uses: "live/agent"},
	}}}
	rs := engine.NewRunState("run-loop-exit", "digest", nil)
	rs.RecordLoopIter("loop[0]", 1)
	rs.RecordCompleted("loop[0].body.iter-1.check", engine.NodeResult{
		Outcome: engine.OutcomeOK,
		Outputs: map[string]any{"done": true},
	})

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].NodePath != "after" {
		t.Fatalf("requests = %+v, want following live request", reqs)
	}
}

func TestLiveResumePreflightCompletedParallelContinuesToFollowingLive(t *testing.T) {
	t.Parallel()

	live := fake.New("live/agent").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.Parallel{Children: ir.NodeList{
			&ir.CodeStep{ID: "left"},
			&ir.CodeStep{ID: "right"},
		}},
		&ir.AgentStep{ID: "after", Uses: "live/agent"},
	}}}
	rs := engine.NewRunState("run-parallel-done", "digest", nil)
	rs.RecordCompleted("parallel[0].left", engine.NodeResult{Outcome: engine.OutcomeOK})
	rs.RecordCompleted("parallel[0].right", engine.NodeResult{Outcome: engine.OutcomeOK})

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].NodePath != "after" {
		t.Fatalf("requests = %+v, want following live request", reqs)
	}
}

func TestLiveResumePreflightCompletedMapContinuesToFollowingLive(t *testing.T) {
	t.Parallel()

	live := fake.New("live/agent").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.Map{
			Over:        "{{ input.items }}",
			As:          "item",
			Container:   "worker",
			Concurrency: intPtr(2),
			Body:        ir.NodeList{&ir.CodeStep{ID: "scan"}},
		},
		&ir.AgentStep{ID: "after", Uses: "live/agent"},
	}}}
	rs := engine.NewRunState("run-map-done", "digest", map[string]any{
		"items": []any{"a", "b"},
	})
	rs.RecordMapItem("map[0]", engine.MapItemRecord{N: 0, Status: engine.ItemPassed})
	rs.RecordMapItem("map[0]", engine.MapItemRecord{N: 1, Status: engine.ItemPassed})

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].NodePath != "after" {
		t.Fatalf("requests = %+v, want following live request", reqs)
	}
}

func TestLiveResumePreflightCollectsAllUncommittedMapItems(t *testing.T) {
	t.Parallel()

	live := fake.New("live/agent").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.Map{
			Over:        "{{ input.items }}",
			As:          "item",
			Container:   "worker",
			Concurrency: intPtr(2),
			Body: ir.NodeList{
				&ir.CodeStep{ID: "prep"},
				&ir.AgentStep{ID: "live", Uses: "live/agent"},
			},
		},
	}}}
	rs := engine.NewRunState("run-map-mixed", "digest", map[string]any{
		"items": []any{"strict-first", "live-second"},
	})
	rs.RecordCompleted("map[0].item-1.prep", engine.NodeResult{Outcome: engine.OutcomeOK})

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].NodePath != "map[0].item-1.live" {
		t.Fatalf("requests = %+v, want live request from second uncommitted item", reqs)
	}
}

func TestLiveResumePreflightWalksComposeBody(t *testing.T) {
	t.Parallel()

	live := fake.New("live/agent").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.Compose{
			As:      "runtime",
			From:    "step.make.files.compose",
			Service: "app",
			Body:    ir.NodeList{&ir.AgentStep{ID: "live", Uses: "live/agent"}},
		},
	}}}
	rs := engine.NewRunState("run-compose", "digest", nil)

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].NodePath != "compose[0].body.live" {
		t.Fatalf("requests = %+v, want compose body live request", reqs)
	}
}

func TestLiveResumePreflightStopsAtGateAttemptFrontier(t *testing.T) {
	t.Parallel()

	live := fake.New("live/agent").WithCaps(agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	})
	var reg agent.Registry
	if err := reg.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ld := &ir.LoadedDefinition{Workflow: &ir.Workflow{Graph: ir.NodeList{
		&ir.Gate{
			Generate: ir.NodeList{&ir.CodeStep{ID: "draft"}},
			Evaluate: ir.NodeList{&ir.CodeStep{
				ID:           "judge",
				OutputSchema: &ir.JSONSchema{"type": "object"},
			}},
			Until:       "{{ evaluate.passed }}",
			MaxAttempts: 2,
		},
		&ir.AgentStep{ID: "after", Uses: "live/agent"},
	}}}
	rs := engine.NewRunState("run-gate-frontier", "digest", nil)
	rs.RecordCompleted("gate[0].attempt-1.generate.draft", engine.NodeResult{Outcome: engine.OutcomeOK})
	rs.RecordCompleted("gate[0].attempt-1.evaluate.judge", engine.NodeResult{
		Outcome: engine.OutcomeOK,
		Outputs: map[string]any{"passed": true},
	})

	reqs, err := engine.LiveResumePreflightRequests(ld, rs, &reg)
	if err != nil {
		t.Fatalf("LiveResumePreflightRequests: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatalf("requests = %+v, want none before gate.attempt commits", reqs)
	}
}
