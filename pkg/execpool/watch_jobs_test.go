package execpool

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/fundi/tools"

	"github.com/multigres/testkit/assert"
)

// jobWatchFake is a minimal executor service for the WatchJobs tests. Each
// WatchJobs call increments a counter and invokes onSubscribe with the 1-based
// subscription number and the stream, so a test scripts per-subscription
// behaviour (replay once, fail once, and so on).
type jobWatchFake struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	executorID string

	onSubscribe func(ctx context.Context, n int, stream *connect.ServerStream[executorpb.WatchJobsResponse]) error

	mu    sync.Mutex
	calls int
	subs  chan int // optional: one send per WatchJobs call
}

func (f *jobWatchFake) Describe(context.Context, *connect.Request[executorpb.DescribeRequest]) (*connect.Response[executorpb.DescribeResponse], error) {
	return connect.NewResponse(&executorpb.DescribeResponse{
		ExecutorId: f.executorID,
		Tools:      []string{"read", "bash"},
	}), nil
}

func (f *jobWatchFake) Health(context.Context, *connect.Request[executorpb.HealthRequest]) (*connect.Response[executorpb.HealthResponse], error) {
	return connect.NewResponse(&executorpb.HealthResponse{}), nil
}

func (f *jobWatchFake) WatchJobs(ctx context.Context, _ *connect.Request[executorpb.WatchJobsRequest], stream *connect.ServerStream[executorpb.WatchJobsResponse]) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.subs != nil {
		select {
		case f.subs <- n:
		default:
		}
	}
	if f.onSubscribe == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.onSubscribe(ctx, n, stream)
}

func watchRunning(handle, owner, command string) *executorpb.WatchJobsResponse {
	return &executorpb.WatchJobsResponse{
		Event: &executorpb.WatchJobsResponse_Running{Running: &executorpb.JobRunning{
			Handle: handle, Owner: owner, Command: command,
		}},
	}
}

func watchExited(handle, owner, command string, code int32) *executorpb.WatchJobsResponse {
	return &executorpb.WatchJobsResponse{
		Event: &executorpb.WatchJobsResponse_Exited{Exited: &executorpb.JobExited{
			Handle: handle, Owner: owner, Command: command, ExitCode: code,
		}},
	}
}

func watchReplayComplete() *executorpb.WatchJobsResponse {
	return &executorpb.WatchJobsResponse{
		Event: &executorpb.WatchJobsResponse_ReplayComplete{ReplayComplete: &executorpb.ReplayComplete{}},
	}
}

func (f *jobWatchFake) subscriptionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// The executor call deadline must clear the bash tool's promote window. The
// executor applies ExecuteRequest.Timeout as the call's context deadline, and a
// bash call that reaches its clamped timeout_ms is promoted by a timer firing
// at exactly that timeout: a deadline at or below it races the promote (the
// select's coin-flip), so a command that should become a background job gets
// killed instead. Pin executorCallTimeout STRICTLY above the bash max, with a
// comfortable margin (not merely send_steps.go's 5s drain slack).
func TestExecutorCallTimeoutClearsTheBashPromoteWindow(t *testing.T) {
	ck := assert.NewAborting(t)
	ck.Greater(tools.MaxBashTimeout, executorCallTimeout,
		"executorCallTimeout must STRICTLY exceed the bash max timeout, or a max-timeout bash call is killed instead of promoted")
	// A literal floor, not the constant's own definition restated: this fails if
	// the margin shrinks below 10s, regardless of how the constant is written.
	ck.GreaterOrEqual(10*time.Second, executorCallTimeout-tools.MaxBashTimeout,
		"the margin over the bash max must be at least 10s")
}

// The events an executor sends arrive at the daemon's handler in order, on one
// goroutine, with their fields intact. The FIRST event on a fresh subscription
// is the synthetic ReplayStart watchJobs emits at each successful open, before
// any replayed event.
func TestWatchJobsDeliversEventsInOrder(t *testing.T) {
	ck := assert.NewAborting(t)
	p := New(newFakeStore("exec-jobs"))

	got := make(chan JobEvent, 16)
	p.SetJobEventHandler(func(ev JobEvent) { got <- ev })

	fake := &jobWatchFake{executorID: "exec-jobs"}
	fake.onSubscribe = func(ctx context.Context, _ int, stream *connect.ServerStream[executorpb.WatchJobsResponse]) error {
		for _, msg := range []*executorpb.WatchJobsResponse{
			watchRunning("h1", "c1", "make check"),
			watchExited("h1", "c1", "make check", 7),
			watchReplayComplete(),
		} {
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
		<-ctx.Done() // hold the stream open past the ordering assertions
		return ctx.Err()
	}
	ck.NoError(joinViaUpgrade(t, p, fake), "join")
	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })

	kinds := make([]JobEventKind, 0, 4)
	for range 4 {
		select {
		case ev := <-got:
			kinds = append(kinds, ev.Kind)
			if ev.Kind == JobEventExited {
				ck.Eq(7, ev.ExitCode, "exit code")
				ck.Eq("h1", ev.Handle, "handle")
				ck.Eq("c1", ev.Owner, "owner")
				ck.Eq("exec-jobs", ev.ExecutorID, "executor id")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a job event")
		}
	}
	ck.EqDeep([]JobEventKind{JobEventReplayStart, JobEventRunning, JobEventExited, JobEventReplayComplete}, kinds,
		"events must arrive in order, led by the ReplayStart marker")

	p.Evict("exec-jobs")
}

// A stream the executor ends (a fall-behind subscriber is ended with
// CodeUnavailable) is reconnected: a second subscription arrives and its events
// are delivered. This is the one place a wait is allowed — it is reconnection,
// not polling.
func TestWatchJobsResubscribesAfterStreamEnds(t *testing.T) {
	ck := assert.NewAborting(t)
	p := New(newFakeStore("exec-jobs"))

	got := make(chan JobEvent, 16)
	p.SetJobEventHandler(func(ev JobEvent) { got <- ev })

	fake := &jobWatchFake{executorID: "exec-jobs"}
	fake.onSubscribe = func(ctx context.Context, n int, stream *connect.ServerStream[executorpb.WatchJobsResponse]) error {
		if n == 1 {
			// The executor ends the first subscription, as it does a lagging
			// subscriber.
			return connect.NewError(connect.CodeUnavailable, errors.New("subscriber fell behind"))
		}
		if err := stream.Send(watchExited("h2", "c1", "npm test", 3)); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	ck.NoError(joinViaUpgrade(t, p, fake), "join")
	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })

	// The resubscribed stream leads with a ReplayStart marker; drain until the
	// exit it carries arrives.
	var exited *JobEvent
	deadline := time.After(5 * time.Second)
	for exited == nil {
		select {
		case ev := <-got:
			if ev.Kind == JobEventExited {
				e := ev
				exited = &e
			}
		case <-deadline:
			t.Fatal("no exit event arrived after the stream ended; the watcher did not resubscribe")
		}
	}
	ck.Eq("h2", exited.Handle, "handle from the resubscribed stream")
	ck.Greater(1, fake.subscriptionCount(), "want a second subscription, got")

	p.Evict("exec-jobs")
}

// When the connection closes, the watcher stops: no resubscription follows.
func TestWatchJobsStopsWhenConnectionCloses(t *testing.T) {
	ck := assert.NewAborting(t)
	p := New(newFakeStore("exec-jobs"))
	p.SetJobEventHandler(func(JobEvent) {})

	fake := &jobWatchFake{executorID: "exec-jobs", subs: make(chan int, 8)}
	// Default onSubscribe (nil) holds the stream until its context is cancelled.
	ck.NoError(joinViaUpgrade(t, p, fake), "join")
	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })

	select {
	case <-fake.subs:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never subscribed")
	}

	p.Evict("exec-jobs")

	// The initial reconnection backoff is 1s; waiting past it proves no
	// resubscription happened after the connection closed.
	time.Sleep(1500 * time.Millisecond)
	ck.Eq(1, fake.subscriptionCount(), "the watcher must not resubscribe after its connection closes")
}

// ownerRecordFake records what a background-job-aware executor sees on an
// Execute: the request's owner and AutoBackground flag, keyed by tool.
type ownerRecordFake struct {
	executorpbconnect.UnimplementedExecutorServiceHandler

	mu      sync.Mutex
	records []ownerRecord
}

type ownerRecord struct {
	tool           string
	owner          string
	autoBackground bool
	background     bool
}

func (f *ownerRecordFake) Describe(context.Context, *connect.Request[executorpb.DescribeRequest]) (*connect.Response[executorpb.DescribeResponse], error) {
	return connect.NewResponse(&executorpb.DescribeResponse{
		ExecutorId: "exec-owner",
		Tools:      []string{"read", "bash"},
	}), nil
}

func (f *ownerRecordFake) Health(context.Context, *connect.Request[executorpb.HealthRequest]) (*connect.Response[executorpb.HealthResponse], error) {
	return connect.NewResponse(&executorpb.HealthResponse{}), nil
}

func (f *ownerRecordFake) Execute(_ context.Context, req *connect.Request[executorpb.ExecuteRequest], stream *connect.ServerStream[executorpb.ExecuteResponse]) error {
	f.mu.Lock()
	f.records = append(f.records, ownerRecord{
		tool:           req.Msg.GetTool(),
		owner:          req.Msg.GetOwner(),
		autoBackground: req.Msg.GetAutoBackground(),
		background:     req.Msg.GetBackground(),
	})
	f.mu.Unlock()

	if req.Msg.GetBackground() {
		return stream.Send(&executorpb.ExecuteResponse{
			Event: &executorpb.ExecuteResponse_Handle{Handle: "job-1"},
		})
	}
	return stream.Send(&executorpb.ExecuteResponse{
		Event: &executorpb.ExecuteResponse_Result{Result: &executorpb.Result{Content: []*executorpb.ContentBlock{
			{Block: &executorpb.ContentBlock_Text{Text: "ok"}},
		}}},
	})
}

func (f *ownerRecordFake) last() ownerRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.records) == 0 {
		return ownerRecord{}
	}
	return f.records[len(f.records)-1]
}

// The owner and the auto-background flag ride the ExecuteRequest: bash is
// auto-backgroundable, read is not, and StartJob carries the owner too.
func TestOwnerRidesExecuteAndStartJob(t *testing.T) {
	ck := assert.NewAborting(t)
	p := New(newFakeStore("exec-owner"))
	fake := &ownerRecordFake{}
	ck.NoError(joinViaUpgrade(t, p, fake), "join")
	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })

	cl, err := p.ClientFor("exec-owner")
	ck.NoError(err, "ClientFor")
	ctx := WithOwner(context.Background(), "child-9")

	if _, err := cl.Execute(ctx, "bash", json.RawMessage(`{"command":"sleep 30"}`)); err != nil {
		t.Fatalf("bash Execute: %v", err)
	}
	rec := fake.last()
	ck.Eq("bash", rec.tool, "tool")
	ck.Eq("child-9", rec.owner, "the owner must ride the ExecuteRequest")
	ck.True(rec.autoBackground, "bash must be auto-backgroundable")
	ck.False(rec.background, "an ordinary bash Execute is not a background start")

	if _, err := cl.Execute(ctx, "read", json.RawMessage(`{"path":"/tmp/x"}`)); err != nil {
		t.Fatalf("read Execute: %v", err)
	}
	rec = fake.last()
	ck.Eq("read", rec.tool, "tool")
	ck.False(rec.autoBackground, "read must NOT be auto-backgroundable")

	if _, err := cl.StartJob(ctx, "ls"); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	rec = fake.last()
	ck.True(rec.background, "StartJob must set Background")
	ck.Eq("child-9", rec.owner, "the owner must ride StartJob")

	p.Evict("exec-owner")
}

// The recovery gate holds the FIRST WatchJobs open until the daemon closes it.
// Without the hold, an executor reconnecting over the local unix socket (which
// accepts before loadChildren) could replay an unacked exit before its owner is
// loaded, and the handler would drop the fragment and ack it — losing the only
// copy. The gate is not a poll: the watcher waits on a channel the daemon
// closes, and the exit delivered after it opens is the replayed one.
func TestWatchJobsHoldsFirstOpenUntilGateOpens(t *testing.T) {
	ck := assert.NewAborting(t)
	p := New(newFakeStore("exec-jobs"))

	gate := make(chan struct{})
	p.HoldJobEventsUntil(gate)

	got := make(chan JobEvent, 16)
	p.SetJobEventHandler(func(ev JobEvent) { got <- ev })

	fake := &jobWatchFake{executorID: "exec-jobs", subs: make(chan int, 8)}
	fake.onSubscribe = func(ctx context.Context, _ int, stream *connect.ServerStream[executorpb.WatchJobsResponse]) error {
		if err := stream.Send(watchExited("h1", "c1", "make check", 0)); err != nil {
			return err
		}
		<-ctx.Done() // hold the stream open
		return ctx.Err()
	}
	ck.NoError(joinViaUpgrade(t, p, fake), "join")
	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })

	// With the gate held closed, NO WatchJobs subscription may open and no event
	// may reach the handler.
	select {
	case n := <-fake.subs:
		t.Fatalf("WatchJobs opened (subscription %d) while the recovery gate was closed", n)
	case <-time.After(300 * time.Millisecond):
	}
	ck.Eq(0, fake.subscriptionCount(), "no subscription may open while the gate is held")
	select {
	case ev := <-got:
		t.Fatalf("a job event (kind %d) was consumed while the recovery gate was closed", ev.Kind)
	default:
	}

	// Opening the gate lets the subscription open and the replay arrive.
	close(gate)
	select {
	case <-fake.subs:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchJobs never opened after the recovery gate closed")
	}
	var exited *JobEvent
	deadline := time.After(5 * time.Second)
	for exited == nil {
		select {
		case ev := <-got:
			if ev.Kind == JobEventExited {
				e := ev
				exited = &e
			}
		case <-deadline:
			t.Fatal("no exit event arrived after the gate opened")
		}
	}
	ck.Eq("h1", exited.Handle, "handle from the exit delivered after the gate opened")

	p.Evict("exec-jobs")
}
