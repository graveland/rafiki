package execpool

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// ErrNoDeadline is returned by stdioConn's deadline setters. A pipe pair has no
// deadline support, and pretending otherwise by returning nil would promise a
// bound that is not enforced — a caller that set a read deadline expecting it to
// unstick a wedged read would wait forever with no error to explain it.
//
// Nothing calls these with the configuration this package uses; see the comment
// on NewStdioConn for the verification. This error exists so that if that ever
// stops being true, it fails loudly instead of silently.
var ErrNoDeadline = errors.New("execpool: stdio connection does not support deadlines")

// stdioAddr is a stub address. Nothing routes on it — HTTP/2 uses the
// connection it is handed — but net.Conn requires the pair.
type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }

// stdioConn adapts a read/write pipe pair to net.Conn so a subprocess's stdio
// can carry HTTP/2. It is how the in-container tool server is reached: the
// container has no network at all (workspace.Derive sets Network: "none"), so
// there is no socket to connect to, and `docker exec -i` gives bidirectional
// stdio instead. Wrapped as a net.Conn, the existing inversion machinery
// applies unchanged — ServeInverted inside, ClientForConn outside.
//
// Deadlines are the one thing a pipe pair cannot provide, so this type is only
// sound if nothing sets them. Verified against x/net/http2 v0.55.0:
//
//   - http2.Server touches conn deadlines in exactly three places
//     (server.go:308, :1988, :2009) and each is guarded by hs.WriteTimeout > 0
//     or hs.ReadTimeout > 0, taken from ServeConnOpts.BaseConfig. ServeInverted
//     passes no BaseConfig, so both are zero.
//   - writeWithByteTimeout (http2.go:325) returns a plain conn.Write when its
//     timeout is <= 0. That timeout is WriteByteTimeout, which config.go:162
//     propagates only when non-zero and nothing defaults.
//   - http2.Transport never touches conn deadlines. ClientForConn's
//     ReadIdleTimeout/PingTimeout are implemented with timers and PING frames,
//     not socket deadlines — which is what makes them work here at all, and
//     means a wedged container is still detected.
//
// Every one of those call sites also discards the returned error, so even a
// future regression degrades to "the deadline silently does nothing" rather
// than a crash. If deadlines are ever genuinely needed, back this with
// net.Pipe pumped by two goroutines: that supports them natively and costs one
// extra copy per message, which is irrelevant at tool-call volume.
type stdioConn struct {
	r io.ReadCloser
	w io.WriteCloser

	closeOnce sync.Once
	closeErr  error
}

// NewStdioConn wraps a reader and a writer as a net.Conn. r is what the peer
// writes to us (a subprocess's stdout); w is what we write to the peer (its
// stdin).
func NewStdioConn(r io.ReadCloser, w io.WriteCloser) net.Conn {
	return &stdioConn{r: r, w: w}
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// Close closes both halves, once. Teardown arrives from two directions here —
// the HTTP/2 transport giving up on the connection, and Release killing the
// workspace — so it must be idempotent and must not report the second caller a
// failure.
func (c *stdioConn) Close() error {
	c.closeOnce.Do(func() {
		// Writer first: closing the peer's stdin is what tells it to exit, and a
		// peer blocked on read never notices its stdout being closed.
		c.closeErr = errors.Join(c.w.Close(), c.r.Close())
	})
	return c.closeErr
}

func (c *stdioConn) LocalAddr() net.Addr  { return stdioAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr { return stdioAddr{} }

func (c *stdioConn) SetDeadline(time.Time) error      { return ErrNoDeadline }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return ErrNoDeadline }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return ErrNoDeadline }
