package codexlive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/live"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/pricing"
)

// normalizeForPricing turns codex token usage into a pricing.Breakdown, using
// totalTokens as a runtime cache-inclusion oracle. Codex's inputTokens is believed
// to INCLUDE cachedInputTokens (cached ⊂ input), but the app-server schema doesn't
// prove it. So subtract cached from input IFF totalTokens == inputTokens +
// outputTokens (the inclusion identity holds); otherwise (e.g. total == input +
// cached + output, the disjoint case) leave input whole. The bool reports whether
// cached was subtracted, so the caller can warn when it declined to.
func normalizeForPricing(u Usage) (pricing.Breakdown, bool) {
	subtract := u.TotalTokens == u.InputTokens+u.OutputTokens // inclusion oracle
	input := u.InputTokens
	if subtract {
		input -= u.CachedInputTokens
	}
	return pricing.Breakdown{Input: input, Output: u.OutputTokens, CacheRead: u.CachedInputTokens}, subtract
}

func (a *Adapter) Launch(ctx context.Context, _ container.Handle, inv agent.AgentInvocation) (<-chan agent.AgentEvent, <-chan agent.AgentOutcome, error) {
	if err := a.requireRootAndClient(); err != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}
	if err := a.ValidateConfig(inv.With); err != nil {
		return nil, nil, err
	}
	cfg, err := parseConfig(cloneRawConfig(inv.With))
	if err != nil {
		return nil, nil, err
	}
	// F33: session/cwd are optional with-keys; default them from run context
	// here (parseConfig only sees `with`, not RunID/NodePath/WorkflowDir).
	// See defaultSessionKey's doc comment for why this default is
	// epoch-stable, unlike agent/claudesession's sessionUUID.
	if cfg.session == "" {
		cfg.session = defaultSessionKey(inv.RunContext.RunID, inv.NodePath)
	}
	if cfg.cwd == "" {
		if inv.WorkflowDir == "" {
			return nil, nil, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "cwd", Reason: "required: no workflow directory available to default to"}
		}
		cfg.cwd = inv.WorkflowDir
	}
	cfg, err = withCanonicalCWD(cfg)
	if err != nil {
		return nil, nil, err
	}
	cfg.prompt, err = agent.PrependFeedback(cfg.prompt, inv.Feedback)
	if err != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}
	policy, err := parsePermissionPolicy(cfg.permissionRaw)
	if err != nil {
		return nil, nil, err
	}
	info, err := a.client.ProviderInfo(ctx)
	if err != nil {
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}
	session, err := a.prepareSession(ctx, cfg, info)
	if err != nil {
		if errors.Is(err, agent.ErrLiveReplayRequired) {
			return nil, nil, err
		}
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}

	leaseID := live.LeaseID(inv.RunContext.RunID, int(inv.RunContext.NextEpoch), inv.NodePath, cfg.session)
	_, err = live.AcquireLease(a.root, live.LeaseRequest{
		AdapterRef:    AdapterRef,
		SessionKey:    cfg.session,
		OwnerRunID:    inv.RunContext.RunID,
		OwnerPID:      os.Getpid(),
		OwnerNodePath: inv.NodePath,
		OwnerEpoch:    int(inv.RunContext.NextEpoch),
		LeaseID:       leaseID,
		TTLSeconds:    120,
	}, a.clock.Now(), nil)
	if err != nil {
		return nil, nil, err
	}

	intent := live.ActiveTurn{
		Phase:        live.PhaseIntentRecorded,
		RunID:        inv.RunContext.RunID,
		NodePath:     inv.NodePath,
		CurrentEpoch: int(inv.RunContext.CurrentEpoch),
		NextEpoch:    int(inv.RunContext.NextEpoch),
		PromptDigest: promptDigest(cfg.prompt),
		LeaseID:      leaseID,
	}
	if err := live.RecordTurnIntent(a.root, AdapterRef, cfg.session, intent); err != nil {
		_ = live.ReleaseLease(a.root, AdapterRef, cfg.session, leaseID)
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}

	req := TurnStartRequest{
		ThreadID:        session.ID,
		Prompt:          cfg.prompt,
		OutputSchema:    inv.OutputSchema,
		Model:           cfg.model,
		ReasoningEffort: cfg.reasoningEffort,
	}
	turn, clearActiveTurn, err := a.startTurnWithBackoff(ctx, req)
	if err != nil {
		if clearActiveTurn {
			_ = live.ClearActiveTurnIfSafe(a.root, AdapterRef, cfg.session)
		}
		_ = live.ReleaseLease(a.root, AdapterRef, cfg.session, leaseID)
		closeClientIfSupported(a.client)
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}
	intent.Phase = live.PhaseProviderTurnStarted
	intent.ProviderTurnID = turn.TurnID
	if err := live.RecordTurnIntent(a.root, AdapterRef, cfg.session, intent); err != nil {
		_ = live.ReleaseLease(a.root, AdapterRef, cfg.session, leaseID)
		return nil, nil, &agent.ErrAgentLaunch{Cause: err}
	}

	meta := agent.LiveDispatch{
		AdapterRef:     AdapterRef,
		SessionKey:     cfg.session,
		SessionKeyHash: sessionKeyHash(cfg.session),
		LeaseID:        leaseID,
		ActiveTurnID:   activeTurnID(intent),
		ProviderTurnID: turn.TurnID,
		RunID:          inv.RunContext.RunID,
		NodePath:       inv.NodePath,
		Epoch:          inv.RunContext.NextEpoch,
	}

	// Prefer the RESOLVED model the app-server reported on thread/start over the
	// requested cfg.model (which may be empty when the request omitted model).
	model := session.Model
	if model == "" {
		model = cfg.model
	}

	events := make(chan agent.AgentEvent, 16)
	outcomes := make(chan agent.AgentOutcome, 1)
	go a.drainTurn(ctx, events, outcomes, turn, session.ID, model, policy, inv, meta)
	return events, outcomes, nil
}

func (a *Adapter) prepareSession(ctx context.Context, cfg config, info ProviderInfo) (ThreadInfo, error) {
	existing, err := live.ReadSessionRecord(a.root, AdapterRef, cfg.session)
	switch {
	case err == nil:
		if existing.ActiveTurn != nil {
			return ThreadInfo{}, agent.ErrLiveReplayRequired
		}
		next := existing
		next.CWD = cfg.cwd
		next.CanonicalCWD = cfg.canonicalCWD
		next.ProviderBinary = info.Binary
		next.ProviderProtocolSchema = AppServerSchemaDigest()
		next.AdapterVersion = live.FormatVersion(info.Version, AppServerSchemaDigest())
		if err := live.CheckSessionDrift(existing, next); err != nil {
			return ThreadInfo{}, err
		}
		session, rerr := a.client.ResumeThread(ctx, ThreadResumeRequest{
			ThreadID:        existing.ProviderSessionID,
			CWD:             cfg.cwd,
			Model:           cfg.model,
			ReasoningEffort: cfg.reasoningEffort,
		})
		if rerr != nil {
			return ThreadInfo{}, rerr
		}
		if session.ID == "" {
			session.ID = existing.ProviderSessionID
		}
		if err := live.WriteSessionRecord(a.root, next); err != nil {
			return ThreadInfo{}, err
		}
		return session, nil
	case errors.Is(err, os.ErrNotExist):
		session, serr := a.client.StartThread(ctx, ThreadStartRequest{
			CWD:             cfg.cwd,
			Model:           cfg.model,
			ReasoningEffort: cfg.reasoningEffort,
		})
		if serr != nil {
			return ThreadInfo{}, serr
		}
		rec := live.SessionRecord{
			AdapterRef:             AdapterRef,
			SessionKey:             cfg.session,
			CWD:                    cfg.cwd,
			CanonicalCWD:           cfg.canonicalCWD,
			ProviderSessionID:      session.ID,
			TmuxSession:            session.TmuxSession,
			TranscriptPath:         session.TranscriptPath,
			OwnerRunID:             "",
			LastSeenUnix:           a.clock.Now().Unix(),
			AdapterVersion:         live.FormatVersion(info.Version, AppServerSchemaDigest()),
			ProviderBinary:         info.Binary,
			ProviderProtocolSchema: AppServerSchemaDigest(),
		}
		if err := live.WriteSessionRecord(a.root, rec); err != nil {
			return ThreadInfo{}, err
		}
		return session, nil
	default:
		return ThreadInfo{}, err
	}
}

func (a *Adapter) startTurnWithBackoff(ctx context.Context, req TurnStartRequest) (TurnHandle, bool, error) {
	var last error
	for attempt := 0; ; attempt++ {
		turn, err := a.client.StartTurn(ctx, req)
		if err == nil {
			return turn, false, nil
		}
		last = err
		if !isBackpressure(err) {
			return TurnHandle{}, false, last
		}
		if attempt >= len(a.backoff) {
			return TurnHandle{}, true, last
		}
		if sleepErr := a.clock.Sleep(ctx, a.backoff[attempt]); sleepErr != nil {
			return TurnHandle{}, true, sleepErr
		}
	}
}

func (a *Adapter) drainTurn(ctx context.Context, events chan<- agent.AgentEvent, outcomes chan<- agent.AgentOutcome, turn TurnHandle, threadID, model string, policy permissionPolicy, inv agent.AgentInvocation, meta agent.LiveDispatch) {
	defer close(outcomes)
	defer close(events)
	defer closeClientIfSupported(a.client)

	var finalText string
	var output map[string]any
	var usage Usage
	for {
		select {
		case <-ctx.Done():
			outcomes <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
			return
		case ev, ok := <-turn.Events:
			if !ok {
				outcomes <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: errors.New("agent/codexlive: turn stream ended before turn/completed")}}
				return
			}
			switch ev.Type {
			case EventPermissionRequest:
				allowed := policy.allows(ev.Permission)
				requestID := ""
				if ev.Permission != nil {
					requestID = ev.Permission.ID
				}
				if err := a.client.RespondPermission(ctx, PermissionResponse{
					RequestID: requestID,
					ThreadID:  threadID,
					TurnID:    turn.TurnID,
					Allow:     allowed,
					Reason:    permissionReason(allowed),
				}); err != nil {
					outcomes <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: err}}
					return
				}
				if !allowed {
					outcomes <- agent.AgentOutcome{Err: agent.ErrPermissionDenied}
					return
				}
			case EventAgentMessageDelta:
				finalText += ev.Text
				if !sendLiveEvent(ctx, events, ev.Type, ev.Text) {
					outcomes <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
					return
				}
			case EventItemCompleted:
				finalText = ev.Text
				if ev.Text != "" && !sendLiveEvent(ctx, events, ev.Type, ev.Text) {
					outcomes <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: ctx.Err()}}
					return
				}
			case EventThreadTokenUsage:
				// Usage rides its own notification (the turn/completed payload has
				// none); latch the latest before the terminal event commits it.
				usage = ev.Usage
			case EventTurnCompleted:
				if !turnCompletedSuccessfully(ev) {
					outcomes <- agent.AgentOutcome{Err: &agent.ErrAgentLaunch{Cause: fmt.Errorf("agent/codexlive: turn %s ended with status %q: %s", turn.TurnID, ev.Status, ev.Error)}}
					return
				}
				output = ev.Output
				if output == nil && inv.OutputSchema != nil && strings.TrimSpace(finalText) != "" {
					if err := json.Unmarshal([]byte(strings.TrimSpace(finalText)), &output); err != nil {
						outcomes <- agent.AgentOutcome{Err: agent.ErrLiveReplayRequired}
						return
					}
				}
				meta.CommittedUnix = a.clock.Now().Unix()
				ms := agent.MetricSet{
					Tokens: agent.MetricTokens{
						Input:          usage.InputTokens,
						Output:         usage.OutputTokens,
						CacheReadInput: usage.CachedInputTokens,
					},
					Model: model,
				}
				b, subtracted := normalizeForPricing(usage)
				if !subtracted && usage.CachedInputTokens > 0 && usage.TotalTokens != 0 {
					fmt.Fprintf(os.Stderr, "agent/codexlive: tokenUsage totalTokens %d != input+output %d; not subtracting cached %d\n", usage.TotalTokens, usage.InputTokens+usage.OutputTokens, usage.CachedInputTokens)
				}
				if model != "" {
					if c, ok := a.pricer.Derive(model, b); ok {
						ms.Cost = agent.MetricCost{Source: agent.CostSourceDerived, Currency: c.Currency, Total: c.Total, Input: c.Input, Output: c.Output}
					}
				}
				outcomes <- agent.AgentOutcome{Result: agent.AgentResult{
					Output:   output,
					ExitCode: 0,
					Live:     &meta,
					Metrics:  ms,
				}}
				return
			}
		}
	}
}

func turnCompletedSuccessfully(ev ProviderEvent) bool {
	return ev.Status == "" || ev.Status == "completed"
}

type closeClient interface {
	Close() error
}

func closeClientIfSupported(client Client) {
	if closer, ok := client.(closeClient); ok {
		_ = closer.Close()
	}
}

func sendLiveEvent(ctx context.Context, events chan<- agent.AgentEvent, kind, text string) bool {
	payload, err := json.Marshal(map[string]string{
		"type": kind,
		"text": live.RedactKnownSecretShapes(text),
	})
	if err != nil {
		return false
	}
	select {
	case events <- agent.AgentEvent{Kind: kind, Payload: payload, Stream: "live", Live: true}:
		return true
	case <-ctx.Done():
		return false
	}
}

func promptDigest(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sessionKeyHash(session string) string {
	sum := sha256.Sum256([]byte(session))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func activeTurnID(turn live.ActiveTurn) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s", turn.RunID, turn.NodePath, turn.NextEpoch, turn.LeaseID)))
	return "sha256:" + hex.EncodeToString(sum[:])
}
