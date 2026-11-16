package execpool

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// serveConnReadHeaderTimeout bounds the upgrade request's headers on a
// tunnelled connection. Same number as Serve's: after the single connection is
// handed out, an http.Server keeps reading the connection waiting for the next
// request, and without a header deadline a peer that sends one request and then
// goes silent holds the tunnel open forever.
const serveConnReadHeaderTimeout = 10 * time.Second

// ServeConn serves ONE already-established connection through the executor
// endpoint.
//
// This is the install path: a launcher executor opens a tunnel to a remote
// machine and the executor there dials rafikid over it, so the connection
// arrives on a socket nobody accepted. Serving it here rather than through a
// listener keeps it on the SAME handler Serve mounts — the same path, the same
// authorize, the same 101, the same serve — because a tunnelled connection and
// a dialled one must differ in how they arrived and in nothing else. A second
// accept path is exactly the shape this repo has twice shipped a correct guard
// inside of and then routed around.
//
// It returns when the connection's session ends (the peer leaves, or the
// handler closes the connection) or ctx is cancelled, in which case the server
// is closed and the connection with it. A ctx error is returned as ctx.Err();
// an ordinary end returns nil.
func (p *Pool) ServeConn(ctx context.Context, conn net.Conn) error {
	if conn == nil {
		return errors.New("execpool: ServeConn called with no connection")
	}

	mux := http.NewServeMux()
	mux.Handle(upgradeconn.PathFor(upgradeconn.Executor), p.UpgradeHandler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: serveConnReadHeaderTimeout}

	ln := newOneShotListener(conn)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		// Close stops the server accepting and closes the connection it was
		// given. Both are needed: the connection may not have been accepted
		// yet, and once a connection is HIJACKED net/http no longer tracks it,
		// so srv.Close alone would leave the session running.
		_ = srv.Close()
		<-served
		return ctx.Err()
	case <-ln.Done():
		_ = srv.Close()
		return normalizeServeErr(<-served)
	}
}

// normalizeServeErr drops the errors that mean "the one-shot listener is gone,
// as designed" rather than a failure. Closing the listener is the only way
// http.Server.Serve ever stops here, and it reports that as one of these two.
func normalizeServeErr(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// oneShotListener is a net.Listener that yields exactly one connection.
//
// It exists so a connection somebody else established can be served by an
// ordinary http.Server without a second accept loop. The first Accept hands out
// the connection; every Accept after that blocks until the session on it is
// over (the connection has been closed) or the listener is closed, and then
// reports the listener gone — which is what makes http.Server.Serve return.
type oneShotListener struct {
	conn    *oneShotConn
	yielded atomic.Bool
	done    chan struct{} // closed once the yielded connection has been closed
	closed  chan struct{} // closed once Close has been called
	shut    sync.Once
}

func newOneShotListener(conn net.Conn) *oneShotListener {
	l := &oneShotListener{
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	l.conn = &oneShotConn{Conn: conn, onClose: func() { close(l.done) }}
	return l
}

// Accept yields the connection once, then waits for the session to end.
func (l *oneShotListener) Accept() (net.Conn, error) {
	if l.yielded.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	select {
	case <-l.done:
	case <-l.closed:
	}
	return nil, net.ErrClosed
}

// Close stops the listener AND closes the connection it was handed. The
// connection has no other owner: whoever called ServeConn gave it away.
func (l *oneShotListener) Close() error {
	l.shut.Do(func() { close(l.closed) })
	return l.conn.Close()
}

func (l *oneShotListener) Addr() net.Addr {
	if a := l.conn.LocalAddr(); a != nil {
		return a
	}
	// http.Server groups its listeners by address and logs it, so a nil here
	// would be a nil dereference inside net/http rather than a missing label.
	return tunnelAddr{}
}

// tunnelAddr is what Addr reports for a connection with no local address of
// its own — an in-memory pipe, or a conn whose LocalAddr is nil.
type tunnelAddr struct{}

func (tunnelAddr) Network() string { return "tunnel" }
func (tunnelAddr) String() string  { return "tunnel" }

// Done is closed once the connection the listener yielded has been closed —
// that is, once the session it carried is over.
func (l *oneShotListener) Done() <-chan struct{} { return l.done }

// oneShotConn reports its own Close.
//
// net/http is what tells ServeConn a session ended, and only indirectly: it
// closes the connection when the read loop fails, and a hijacked connection is
// closed by the handler that owns it. Either way the Close lands here, which is
// why this wrapper rather than a read probe is the signal — nothing has to
// guess at the protocol on top.
type oneShotConn struct {
	net.Conn
	closeOnce sync.Once
	onClose   func()
}

func (c *oneShotConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(c.onClose)
	return err
}
