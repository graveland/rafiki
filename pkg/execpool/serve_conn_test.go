package execpool

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/upgradeconn"

	"github.com/multigres/testkit/assert"
)

// A connection nobody accepted — an ssh tunnel's far end, in the install path —
// must reach exactly the same admission, authorization and serve as one dialled
// into a listener. Anything less is a second accept path, and a second accept
// path is how a guard gets routed around.
func TestServeConnRunsTheUpgradeHandler(t *testing.T) {
	c := assert.NewCollecting(t)
	store := newFakeStore("row-1")
	store.update(func(e *executors.Executor) {
		e.Labels = map[string]string{"machine": "tunnelled", "owner": "brent"}
	})
	p := New(store)
	p.healthInterval = time.Hour // the row is asserted here, not re-read

	ticket, err := p.Tickets().Mint(TicketGrant{RowID: "row-1"})
	c.Require().NoError(err)

	cliConn, _ := serveConnPipe(t, p)

	// The client half of the exchange, driven from here rather than through
	// execpool.Connect: the executor side of an install dials over a tunnel, so
	// this test must not depend on ConnectOptions.Conn (a sibling change) to
	// reach the pool.
	up, resp, err := upgradeconn.Dial(cliConn, upgradeconn.Executor, "executor", ticketHeader(ticket))
	c.Require().NoError(err, "the tunnelled upgrade was refused")
	c.Eq("row-1", resp.Get(upgradeconn.HeaderExecutorID), "the 101 must name the admitted row, got")

	_, h := executorpbconnect.NewExecutorServiceHandler(&stubHandler{executorID: "row-1"})
	go func() { _ = ServeInverted(t.Context(), up, h) }()

	waitFor(t, 5*time.Second, "the tunnelled executor to go live", func() bool {
		live := p.Live()
		return len(live) == 1 && live[0].Executor.ID == "row-1"
	})
	c.Eq("tunnelled", p.Live()[0].Executor.Labels["machine"],
		"the tunnelled executor must be the admitted ROW, got %v", p.Live()[0].Executor.Labels)
}

// A ticket is one-shot however the connection arrived. The refusal is an
// ordinary HTTP response on the tunnel — no hijack, no session — and ServeConn
// returns once that connection is done rather than holding it open.
func TestServeConnSecondUseRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	// A transient ticket consults no store: stubStore fails every call, so a
	// store read here would refuse the first upgrade and say so.
	p := New(stubStore{})
	p.joinTimeout = 500 * time.Millisecond

	ticket, err := p.Tickets().Mint(TicketGrant{ExecutorID: "sess-1", Owner: "brent"})
	c.Require().NoError(err)

	// First use: the ticket buys the connection, and is spent doing it.
	firstConn, _ := serveConnPipe(t, p)
	if _, _, err := upgradeconn.Dial(firstConn, upgradeconn.Executor, "executor", ticketHeader(ticket)); err != nil {
		t.Fatalf("the first use of a fresh ticket must upgrade: %v", err)
	}
	_ = firstConn.Close()

	// Second use, same ticket, fresh connection — written and read by hand,
	// because a refusal is plain HTTP and there is no upgraded stream to carry.
	secondConn, served := serveConnPipe(t, p)
	req, err := http.NewRequest(http.MethodGet,
		"http://executor"+upgradeconn.PathFor(upgradeconn.Executor), nil)
	c.Require().NoError(err)
	req.Header.Set("Upgrade", string(upgradeconn.Executor))
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set(protocol.EpochHeader, strconv.Itoa(protocol.Epoch))
	req.Header.Set("Authorization", string(upgradeconn.SchemeTicket)+" "+ticket)
	c.Require().NoError(req.Write(secondConn))

	resp, err := http.ReadResponse(bufio.NewReader(secondConn), req)
	c.Require().NoError(err, "no response to the second upgrade")
	c.Eq(http.StatusUnauthorized, resp.StatusCode,
		"a spent ticket must be refused 401 on a second connection, got")
	_ = resp.Body.Close()

	_ = secondConn.Close()
	select {
	case err := <-served:
		c.NoError(err, "ServeConn must return once the session on its connection is over")
	case <-time.After(5 * time.Second):
		t.Fatal("ServeConn did not return after its connection was closed")
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

// serveConnPipe hands ServeConn one half of a net.Pipe and returns the other
// half with the channel its result arrives on. The pipe is the point: there is
// no listener, no address and no accept loop, which is exactly the shape of a
// connection arriving through a tunnel.
func serveConnPipe(t *testing.T, p *Pool) (net.Conn, <-chan error) {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- p.ServeConn(ctx, srvConn) }()
	t.Cleanup(func() {
		cancel()
		_ = cliConn.Close()
	})
	return cliConn, served
}

func ticketHeader(ticket string) http.Header {
	return http.Header{"Authorization": {string(upgradeconn.SchemeTicket) + " " + ticket}}
}
