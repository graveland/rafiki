package execpool

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// JobEventKind classifies one executor job-lifecycle event. Its ZERO value is
// JobEventUnknown, never a meaningful event: a zero-valued JobEvent (a struct
// literal that forgot the field, a decode path that missed) must be ignorable
// rather than misread as a job running — the repo's zero-value-enum rule.
type JobEventKind int

const (
	JobEventUnknown JobEventKind = iota
	JobEventRunning
	JobEventExited
	JobEventReplayComplete
)

// JobEvent is one job event from an executor, routed by Owner.
type JobEvent struct {
	ExecutorID  string
	Kind        JobEventKind
	Handle      string
	WorkspaceID string
	Owner       string
	Command     string
	ExitCode    int
	FinishedAt  time.Time
}

// Reconnection backoff. These bound the wait between a FAILED WatchJobs
// subscription and the next attempt — a reconnection, the one place the
// no-polling rule allows a wait. It is not a poll: the daemon is not asking
// anything, it is retrying a stream that the executor ended.
const (
	jobWatchBackoffInitial = 1 * time.Second
	jobWatchBackoffMax     = 30 * time.Second
)

// SetJobEventHandler registers the daemon's handler for job events. Called once
// at startup, before Serve. Events for one executor are delivered in order on
// one goroutine.
//
// A nil handler disables the subscription entirely: a pool with no daemon
// handler (a test, a component-only pool) opens no WatchJobs stream at all.
func (p *Pool) SetJobEventHandler(fn func(JobEvent)) {
	p.mu.Lock()
	p.jobEventHandler = fn
	p.mu.Unlock()
}

// jobHandler reads the registered handler under the lock.
func (p *Pool) jobHandler() func(JobEvent) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.jobEventHandler
}

// AckJobExits tells executorID its exits for handles are delivered. An unacked
// exit is replayed on every resubscribe and is never evicted, so acking is what
// lets the executor drop it — and it MUST follow the daemon accepting the exit
// (push before ack), never precede it, or a crash between the two loses the
// notification.
func (p *Pool) AckJobExits(ctx context.Context, executorID string, handles []string) error {
	p.mu.RLock()
	lc, ok := p.live[executorID]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("executor %s: %w", executorID, ErrExecutorLost)
	}
	_, err := lc.client.inner.AckJobExits(ctx, connect.NewRequest(&executorpb.AckJobExitsRequest{
		Handles: handles,
	}))
	if err != nil {
		return fmt.Errorf("executor ack job exits: %w", err)
	}
	return nil
}

// watchJobs keeps ONE WatchJobs subscription open for an executor connection
// for the connection's lifetime, reconnecting after a stream ends.
//
// It is started once per connection, next to healthLoop, and runs until
// lc.done closes. It never polls: the executor pushes, the handler is called
// synchronously, and the only wait is the reconnection backoff after a stream
// the executor ended (a fall-behind subscriber is ended with CodeUnavailable,
// and its resubscribe replays everything unacked).
//
// The stream is deliberately NOT counted by inFlight: it is one long-lived
// stream per connection, not a call that can pile up, and counting it would
// leave every executor looking perpetually busy.
func (p *Pool) watchJobs(id string, lc *liveConn) {
	handler := p.jobHandler()
	if handler == nil {
		return
	}

	backoff := jobWatchBackoffInitial
	for {
		gotEvent, err := p.subscribeJobs(id, lc, handler)

		// lc.done closed means the connection is gone: stop, do not resubscribe.
		select {
		case <-lc.done:
			return
		default:
		}

		if gotEvent {
			// Any delivered event resets the backoff: a stream that made
			// progress before ending is not a failing one.
			backoff = jobWatchBackoffInitial
		}
		slog.Warn("execpool: job event stream ended; resubscribing",
			"executor", id, "error", err, "backoff", backoff)

		select {
		case <-time.After(backoff):
		case <-lc.done:
			return
		}
		if !gotEvent {
			backoff = min(backoff*2, jobWatchBackoffMax)
		}
	}
}

// subscribeJobs opens one WatchJobs stream and delivers its events, returning
// whether any event was received and the error that ended it. The stream's
// context is cancelled the moment the connection goes away, so a dead
// connection's subscription unwinds rather than blocking on a peer that will
// never send.
func (p *Pool) subscribeJobs(id string, lc *liveConn, handler func(JobEvent)) (bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-lc.done:
			cancel()
		case <-stop:
		}
	}()

	stream, err := lc.client.inner.WatchJobs(ctx, connect.NewRequest(&executorpb.WatchJobsRequest{}))
	if err != nil {
		return false, err
	}
	defer stream.Close()

	got := false
	for stream.Receive() {
		ev, ok := jobEventFrom(id, stream.Msg())
		if !ok {
			continue
		}
		got = true
		handler(ev)
	}
	return got, stream.Err()
}

// jobEventFrom converts one wire message into a JobEvent. ok is false for a
// message carrying no recognized event (an unknown oneof member, which a
// newer executor could send): the caller skips it rather than misreading it as
// the zero-value kind.
func jobEventFrom(executorID string, msg *executorpb.WatchJobsResponse) (JobEvent, bool) {
	switch ev := msg.GetEvent().(type) {
	case *executorpb.WatchJobsResponse_Running:
		r := ev.Running
		return JobEvent{
			ExecutorID:  executorID,
			Kind:        JobEventRunning,
			Handle:      r.GetHandle(),
			WorkspaceID: r.GetWorkspaceId(),
			Owner:       r.GetOwner(),
			Command:     r.GetCommand(),
		}, true
	case *executorpb.WatchJobsResponse_Exited:
		e := ev.Exited
		var finished time.Time
		if ts := e.GetFinishedAt(); ts != nil {
			finished = ts.AsTime()
		}
		return JobEvent{
			ExecutorID:  executorID,
			Kind:        JobEventExited,
			Handle:      e.GetHandle(),
			WorkspaceID: e.GetWorkspaceId(),
			Owner:       e.GetOwner(),
			Command:     e.GetCommand(),
			ExitCode:    int(e.GetExitCode()),
			FinishedAt:  finished,
		}, true
	case *executorpb.WatchJobsResponse_ReplayComplete:
		return JobEvent{ExecutorID: executorID, Kind: JobEventReplayComplete}, true
	default:
		return JobEvent{}, false
	}
}
