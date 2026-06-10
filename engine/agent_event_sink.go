package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/state"
)

var ErrAgentEventSinkBackpressure = errors.New("engine: agent event sink backpressure")

const agentEventSinkBuffer = 64
const agentEventDeltaFlushDelay = 25 * time.Millisecond

type agentEventSink struct {
	ctx            context.Context
	cancel         context.CancelFunc
	cancelProvider context.CancelFunc
	clk            clock.Clock
	log            state.Log
	blobs          state.Blobs
	path           string
	events         chan agent.AgentEvent
	done           chan error
	flushReq       chan chan error
	pendingDelta   *agent.AgentEvent

	mu  sync.Mutex
	err error
}

func newAgentEventSink(parent context.Context, cancelProvider context.CancelFunc, clk clock.Clock, log state.Log, blobs state.Blobs, path string) *agentEventSink {
	ctx, cancel := context.WithCancel(parent)
	s := &agentEventSink{
		ctx:            ctx,
		cancel:         cancel,
		cancelProvider: cancelProvider,
		clk:            clk,
		log:            log,
		blobs:          blobs,
		path:           path,
		events:         make(chan agent.AgentEvent, agentEventSinkBuffer),
		done:           make(chan error, 1),
		flushReq:       make(chan chan error),
	}
	go s.run()
	return s
}

func (s *agentEventSink) send(ev agent.AgentEvent) error {
	select {
	case <-s.ctx.Done():
		return s.currentErr()
	default:
	}
	select {
	case s.events <- ev:
		return nil
	case <-s.ctx.Done():
		return s.currentErr()
	default:
		err := fmt.Errorf("%w at %q", ErrAgentEventSinkBackpressure, s.path)
		s.setErr(err)
		return err
	}
}

func (s *agentEventSink) closeWait() error {
	close(s.events)
	return <-s.done
}

func (s *agentEventSink) flushWait() error {
	select {
	case <-s.ctx.Done():
		return s.currentErr()
	default:
	}
	ack := make(chan error, 1)
	select {
	case s.flushReq <- ack:
	case <-s.ctx.Done():
		return s.currentErr()
	}
	select {
	case err := <-ack:
		return err
	case <-s.ctx.Done():
		return s.currentErr()
	}
}

func (s *agentEventSink) run() {
	deltaReady := make(chan uint64, 1)
	var deltaTimerCancel context.CancelFunc
	var deltaTimerToken uint64
	stopDeltaTimer := func() {
		if deltaTimerCancel == nil {
			return
		}
		deltaTimerCancel()
		deltaTimerCancel = nil
	}
	startDeltaTimer := func() {
		if deltaTimerCancel != nil {
			return
		}
		timerCtx, cancel := context.WithCancel(s.ctx)
		deltaTimerCancel = cancel
		deltaTimerToken++
		token := deltaTimerToken
		go func() {
			if err := s.clk.Sleep(timerCtx, agentEventDeltaFlushDelay); err != nil {
				return
			}
			select {
			case deltaReady <- token:
			case <-s.ctx.Done():
			}
		}()
	}
	defer stopDeltaTimer()
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				stopDeltaTimer()
				if err := s.flushPendingDelta(); err != nil {
					s.done <- err
					return
				}
				s.done <- s.currentErrNilOK()
				return
			}
			if ev.Live && ev.Display.Class == agent.DisplayAssistantDelta {
				s.mergeDelta(ev)
				startDeltaTimer()
				continue
			}
			stopDeltaTimer()
			if err := s.handleEvent(ev); err != nil {
				s.done <- err
				return
			}
		case ack := <-s.flushReq:
			stopDeltaTimer()
			if err := s.drainBufferedEvents(); err != nil {
				ack <- err
				s.done <- err
				return
			}
			if err := s.flushPendingDelta(); err != nil {
				ack <- err
				s.done <- err
				return
			}
			ack <- s.currentErrNilOK()
		case token := <-deltaReady:
			if token != deltaTimerToken {
				continue
			}
			deltaTimerCancel = nil
			if err := s.flushPendingDelta(); err != nil {
				s.done <- err
				return
			}
		case <-s.ctx.Done():
			s.done <- s.currentErr()
			return
		}
	}
}

func (s *agentEventSink) drainBufferedEvents() error {
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				return nil
			}
			if err := s.handleEvent(ev); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (s *agentEventSink) handleEvent(ev agent.AgentEvent) error {
	if ev.Live && ev.Display.Class == agent.DisplayAssistantDelta {
		s.mergeDelta(ev)
		return nil
	}
	if err := s.flushPendingDelta(); err != nil {
		return err
	}
	return s.appendOne(ev)
}

func (s *agentEventSink) mergeDelta(ev agent.AgentEvent) {
	if s.pendingDelta == nil {
		pending := ev
		pending.Payload = append([]byte(nil), ev.Payload...)
		s.pendingDelta = &pending
		return
	}
	s.pendingDelta.Payload = append(s.pendingDelta.Payload, ev.Payload...)
	s.pendingDelta.Display.Text += ev.Display.Text
	s.pendingDelta.Kind = ev.Kind
	s.pendingDelta.Stream = ev.Stream
}

func (s *agentEventSink) flushPendingDelta() error {
	if s.pendingDelta == nil {
		return nil
	}
	ev := *s.pendingDelta
	s.pendingDelta = nil
	return s.appendOne(ev)
}

func (s *agentEventSink) appendOne(ev agent.AgentEvent) error {
	if err := appendAgentEvents(s.log, s.blobs, s.path, []agent.AgentEvent{ev}); err != nil {
		wrapped := fmt.Errorf("append live agent.event at %q: %w", s.path, err)
		s.setErr(wrapped)
		return wrapped
	}
	return nil
}

func (s *agentEventSink) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.cancel()
	s.cancelProvider()
}

func (s *agentEventSink) currentErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return s.ctx.Err()
}

func (s *agentEventSink) currentErrNilOK() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
