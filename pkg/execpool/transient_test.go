package execpool

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
)

// stubStore fails every call. A transient executor must never consult it.
type stubStore struct{ executors.Store }

func (stubStore) Get(context.Context, string) (executors.Executor, error) {
	return executors.Executor{}, errors.New("a transient executor must never reach the store")
}

func TestRefreshRowSkipsTransientExecutors(t *testing.T) {
	p := &Pool{
		store:         stubStore{},
		live:          map[string]*liveConn{},
		parked:        map[string]*parkedEntry{},
		healthTimeout: time.Second,
	}
	lc := &liveConn{
		executor:  executors.Executor{ID: "e1", Enabled: true},
		transient: true,
	}
	p.live["e1"] = lc

	if err := p.refreshRow(context.Background(), "e1", lc); err != nil {
		t.Fatalf("a transient executor has no row to re-read; refreshRow must "+
			"skip it entirely or ErrNotFound revokes it on the first health "+
			"tick, 30 seconds in: %v", err)
	}
}

func TestRefreshRowStillRevokesDurableExecutors(t *testing.T) {
	p := &Pool{
		store:         stubStore{},
		live:          map[string]*liveConn{},
		parked:        map[string]*parkedEntry{},
		healthTimeout: time.Second,
	}
	lc := &liveConn{executor: executors.Executor{ID: "e2", Enabled: true}}
	p.live["e2"] = lc

	if err := p.refreshRow(context.Background(), "e2", lc); err != nil {
		t.Fatalf("an unreadable row keeps the last known one: %v", err)
	}
}

// countingStoreWithHook records TouchSeen calls so a test can assert.
type countingStoreWithHook struct {
	stubStore
	onTouch func()
}

func newCountingStore(onTouch func()) *countingStoreWithHook {
	return &countingStoreWithHook{onTouch: onTouch}
}

func (c *countingStoreWithHook) TouchSeen(_ context.Context, _ string) error {
	c.onTouch()
	return nil
}

// L1: executors.id is UUID. TouchSeen must be guarded by !transient in
// healthCheck (handleConn already had the guard). Tested via net.Pipe()
// with an h2 server on one side and an h2 transport on the other, the same
// arrangement handleConn uses — no TLS needed.
func TestHealthCheckDoesNotTouchSeenATransientExecutor(t *testing.T) {
	var touched int
	p := &Pool{
		store:         newCountingStore(func() { touched++ }),
		live:          map[string]*liveConn{},
		parked:        map[string]*parkedEntry{},
		evicted:       map[string]time.Time{},
		healthTimeout: time.Second,
	}
	ec := testH2Client(t, "sess-1")
	lc := &liveConn{executor: executors.Executor{ID: "sess-1", Enabled: true}, transient: true, client: ec}
	p.live["sess-1"] = lc
	_ = p.healthCheck(context.Background(), "sess-1", lc)
	if touched != 0 {
		t.Fatalf("TouchSeen called %d times for a row-less executor", touched)
	}
}

func TestHealthCheckDoesTouchSeenADurableExecutor(t *testing.T) {
	var touched int
	p := &Pool{
		store:         newCountingStore(func() { touched++ }),
		live:          map[string]*liveConn{},
		parked:        map[string]*parkedEntry{},
		evicted:       map[string]time.Time{},
		healthTimeout: time.Second,
	}
	ec := testH2Client(t, "durable-1")
	lc := &liveConn{executor: executors.Executor{ID: "durable-1", Enabled: true}, client: ec}
	p.live["durable-1"] = lc
	_ = p.healthCheck(context.Background(), "durable-1", lc)
	if touched == 0 {
		t.Fatal("a durable executor must still get TouchSeen on every health check")
	}
}

// testH2Client creates a working executorClient over a net.Pipe, the same
// way handleConn does — h2 transport on one side, h2 server on the other.
func testH2Client(t *testing.T, executorID string) *executorClient {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	t.Cleanup(func() { srvConn.Close(); cliConn.Close() })

	// Start an h2 server on srvConn, the "executor" side.
	_, handler := executorpbconnect.NewExecutorServiceHandler(
		&stubHandler{executorID: executorID},
	)
	srv := &http2.Server{}
	go srv.ServeConn(srvConn, &http2.ServeConnOpts{Handler: handler})

	// Create an h2 transport on cliConn, the "daemon" side.
	httpClient, err := ClientForConn(cliConn)
	if err != nil {
		t.Fatalf("ClientForConn: %v", err)
		return nil
	}
	inner := executorpbconnect.NewExecutorServiceClient(httpClient, "http://executor")
	return &executorClient{inner: inner}
}

// L2: CLAUDE.md: Pool.live must be mutated on connection IDENTITY, never on
// executor ID alone. Evict is the one place that broke the rule.
func TestEvictDoesNotCloseAReplacementConnection(t *testing.T) {
	p := New(stubStore{})
	old := &liveConn{executor: executors.Executor{ID: "e1"}, done: make(chan struct{})}
	p.live["e1"] = old
	replacement := &liveConn{executor: executors.Executor{ID: "e1"}, done: make(chan struct{})}
	p.live["e1"] = replacement

	p.evictConn("e1", old) // evicting the stale one
	if _, ok := p.live["e1"]; !ok {
		t.Fatal("evicting a stale connection must not remove its replacement")
	}
	select {
	case <-replacement.done:
		t.Fatal("evicting the stale connection must not close the replacement")
	default:
	}
}

// L3: The control connection can close between Redeem and installLive — a
// window that spans the Describe join timeout. Revoke is then a no-op and
// Evict finds nothing live, so the executor installs afterwards and, because
// refreshRow skips transients, is never reaped.
func TestATicketRevokedDuringJoinDoesNotGoLive(t *testing.T) {
	p := New(stubStore{})
	ticket, err := p.Tickets().Mint(TicketGrant{ExecutorID: "sess-1", Owner: "brent"})
	if err != nil {
		t.Fatal(err)
	}
	_, ok := p.tickets.Redeem(ticket)
	if !ok {
		t.Fatal("redeem")
	}
	p.Evict("sess-1") // the control connection closes here

	if p.installTransient("sess-1", &liveConn{done: make(chan struct{})}) {
		t.Fatal("an executor whose owning connection already closed must not " +
			"install; nothing else will ever reap it")
	}
}

// The evicted tombstone must be swept so it does not grow without bound.
func TestEvictedTombstonesAreSwept(t *testing.T) {
	p := New(stubStore{})
	p.evicted["sess-old"] = time.Now().Add(-2 * parkTimeout)

	p.sweepParkedOnce(time.Now())
	if _, ok := p.evicted["sess-old"]; ok {
		t.Fatal("evicted entries older than parkTimeout must be swept")
	}
}