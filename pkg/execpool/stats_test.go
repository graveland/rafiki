package execpool

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
)

func TestStatsReportLiveExecutorsParkedAndInFlight(t *testing.T) {
	c := assert.NewAborting(t)
	p := New(nil)
	connected := time.Unix(1_000, 0)
	named := &liveConn{
		done:        make(chan struct{}),
		executor:    executors.Executor{ID: "id-1", Labels: map[string]string{"machine": "silvershift"}},
		client:      &executorClient{inflight: &inFlight{id: "id-1"}},
		network:     "tcp",
		connectedAt: connected,
	}
	named.client.inflight.n.Store(7)
	p.installLive("id-1", named)
	p.installLive("sess-X", &liveConn{done: make(chan struct{}), transient: true, draining: true})
	p.Park("gone", time.Minute)

	st := p.Stats()
	c.Eq(2, len(st.Executors), "both live executors listed")
	c.Eq(1, st.Parked, "one parked")
	c.Eq(uint64(2), st.Connects, "two installs")
	byID := map[string]ExecutorStat{}
	for _, e := range st.Executors {
		byID[e.ID] = e
	}
	c.Eq("silvershift", byID["id-1"].Name, "machine label names the executor")
	c.Eq(int64(7), byID["id-1"].InFlight, "in-flight comes from the connection's counter")
	c.Eq(connected, byID["id-1"].ConnectedAt, "connected-at")
	c.Eq("sess-X", byID["sess-X"].Name, "no machine label falls back to the id")
	c.True(byID["sess-X"].Transient && byID["sess-X"].Draining, "transient and draining flags")
	c.Eq(uint64(1), st.Parks, "park counted")
}

func TestStatsCountDisplacementHealthFailureAndParkExpiry(t *testing.T) {
	c := assert.NewAborting(t)
	p := New(nil)
	first := &liveConn{done: make(chan struct{})}
	p.installLive("e", first)
	second := &liveConn{done: make(chan struct{})}
	p.installLive("e", second)
	c.Eq(uint64(1), p.Stats().Displaced, "a reconnect displaces the previous connection")

	p.onHealthFailure("e", second)
	c.Eq(uint64(1), p.Stats().HealthFailures, "health failure counted")

	// A stale connection's failure removes nothing and must not be counted.
	p.onHealthFailure("e", first)
	c.Eq(uint64(1), p.Stats().HealthFailures, "stale failure not counted")

	p.sweepParkedOnce(time.Now().Add(2 * parkTimeout))
	c.Eq(uint64(1), p.Stats().ParkExpiries, "expired park counted")
	c.Eq(0, p.Stats().Parked, "and removed")
}

func TestCountJobEventCountsOnlyRealKinds(t *testing.T) {
	c := assert.NewAborting(t)
	var pc poolCounters
	for _, k := range []JobEventKind{JobEventRunning, JobEventExited, JobEventExited, JobEventReplayComplete, JobEventUnknown, JobEventReplayStart} {
		pc.countJobEvent(k)
	}
	c.Eq(uint64(1), pc.jobRunning.Load(), "running")
	c.Eq(uint64(2), pc.jobExited.Load(), "exited")
	c.Eq(uint64(1), pc.jobReplayComplete.Load(), "replay complete")
}

// fakeExecutorClient answers only the calls the job-watch paths make; any other
// method panics through the nil embedded interface, which is the point: the
// test must not drift onto calls it does not model.
type fakeExecutorClient struct {
	executorpbconnect.ExecutorServiceClient
	ackErr error
	watch  func() error
}

func (f *fakeExecutorClient) AckJobExits(context.Context, *connect.Request[executorpb.AckJobExitsRequest]) (*connect.Response[executorpb.AckJobExitsResponse], error) {
	if f.ackErr != nil {
		return nil, f.ackErr
	}
	return connect.NewResponse(&executorpb.AckJobExitsResponse{}), nil
}

func (f *fakeExecutorClient) WatchJobs(context.Context, *connect.Request[executorpb.WatchJobsRequest]) (*connect.ServerStreamForClient[executorpb.WatchJobsResponse], error) {
	return nil, f.watch()
}

func TestAckJobExitsCountsCallsAndFailures(t *testing.T) {
	c := assert.NewAborting(t)
	p := New(nil)
	fake := &fakeExecutorClient{}
	p.live["e"] = &liveConn{done: make(chan struct{}), client: &executorClient{inner: fake}}

	c.NoError(p.AckJobExits(context.Background(), "e", []string{"h1"}), "ack")
	st := p.Stats()
	c.Eq(uint64(1), st.JobAcks, "one ack")
	c.Eq(uint64(0), st.JobAckErrors, "no error")

	fake.ackErr = errors.New("executor gone")
	c.Error(p.AckJobExits(context.Background(), "e", []string{"h2"}), "failing ack")
	st = p.Stats()
	c.Eq(uint64(2), st.JobAcks, "two acks")
	c.Eq(uint64(1), st.JobAckErrors, "one failed")

	// An unknown executor never reaches the wire and is neither.
	c.Error(p.AckJobExits(context.Background(), "nope", []string{"h3"}), "unknown executor")
	c.Eq(uint64(2), p.Stats().JobAcks, "an ack that was never sent is not counted")
}

func TestWatchJobsCountsAnEndedStream(t *testing.T) {
	c := assert.NewAborting(t)
	p := New(nil)
	p.SetJobEventHandler(func(JobEvent) {})
	lc := &liveConn{
		done:   make(chan struct{}),
		client: &executorClient{inner: &fakeExecutorClient{watch: func() error { return errors.New("stream refused") }}},
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		p.watchJobs("e", lc)
	}()

	c.Eventually(5*time.Second, "an ended subscription is counted before the backoff", func() error {
		if p.Stats().JobStreamEnds < 1 {
			return errors.New("no stream end counted yet")
		}
		return nil
	})
	lc.shutdown()
	<-finished
}
