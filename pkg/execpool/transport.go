// Package execpool carries the reverse-dialled executor transport and the
// live-connection registry built on it.
//
// The arrangement in one sentence: the executor DIALS rafikid and then SERVES
// HTTP/2 on the connection it dialled, while rafikid ACCEPTS and is the HTTP
// client. TLS roles follow the TCP direction; HTTP roles invert on top.
//
// Dial-out is required, not stylistic: rafikid in k8s cannot reach a laptop
// behind NAT. Nothing needs to be executor-initiated — join is rafikid calling
// Describe on accept, heartbeat is rafikid polling Health, and leave is a typed
// Draining error on the next Execute, learned at dispatch rather than after a
// polling interval.
package execpool

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
)

// ErrRedialed is returned by the dial hook when the HTTP/2 transport asks for
// a second connection.
//
// This is NOT an error to swallow. The hook hands its connection over exactly
// once; a second request means the transport has decided the connection is
// unusable, which in production is the executor going away. Retrying here
// would dial a URL that names nothing — the "address" is a fiction, the
// connection was handed in.
var ErrRedialed = errors.New("execpool: transport requested a second connection; the executor connection is gone")

// ALPNProtocols is what BOTH sides advertise on the shared TLS listener.
//
// It is http/1.1, not h2, and that is not a downgrade: the connection starts as
// an ordinary HTTP/1.1 request that is UPGRADED to the executor protocol, and
// net/http can only hijack an HTTP/1.1 connection. The inverted HTTP/2 begins
// after the 101 response, directly on the byte stream, where ALPN plays no part
// at all.
//
// ALPN therefore stopped being the thing that guarantees both sides agree — the
// Upgrade header is, and it says so in words, failing with a readable HTTP
// status rather than a TLS alert. Exported so both sides cannot spell it
// differently.
var ALPNProtocols = []string{"http/1.1"}

// ServeInverted runs an HTTP/2 server on a connection this process DIALLED.
// Executor side.
func ServeInverted(conn net.Conn, handler http.Handler) error {
	srv := &http2.Server{}
	srv.ServeConn(conn, &http2.ServeConnOpts{
		Handler: handler,
		Context: context.Background(),
	})
	return nil
}

// ClientForConn returns an http.Client that speaks HTTP/2 over exactly this
// already-established connection. rafikid side.
//
// DialTLSContext is the hook even when conn carries no TLS: with
// AllowHTTP: true the transport still routes an http:// URL through the TLS
// dial hook. There is no plain DialContext on http2.Transport — looking for
// one is a dead end that costs an afternoon.
func ClientForConn(conn net.Conn) (*http.Client, error) {
	var handedOver atomic.Bool
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(context.Context, string, string, *tls.Config) (net.Conn, error) {
			if handedOver.Swap(true) {
				return nil, ErrRedialed
			}
			return conn, nil
		},
		// Keepalive, not tuning. This connection was DIALLED BY THE EXECUTOR,
		// so when a laptop sleeps or a NAT drops its mapping there is no FIN
		// and no RST — the socket stays open on our side and every write
		// disappears. Without a PING probe the failure surfaces only when TCP
		// retransmission finally gives up, on the order of fifteen minutes,
		// during which every Execute on that executor hangs.
		//
		// With these, an unreachable peer is detected in at most
		// ReadIdleTimeout+PingTimeout and the transport closes the
		// connection, which turns the next call into a prompt error and lets
		// the health loop park the executor while its park window is still
		// useful.
		ReadIdleTimeout: h2ReadIdleTimeout,
		PingTimeout:     h2PingTimeout,
	}
	return &http.Client{Transport: tr}, nil
}

const (
	// h2ReadIdleTimeout is how long the connection may be silent before the
	// transport probes it. Health polls every 30s, so a connection idle
	// beyond this is already anomalous.
	h2ReadIdleTimeout = 15 * time.Second
	// h2PingTimeout is how long a PING may go unacknowledged before the
	// connection is declared dead.
	h2PingTimeout = 10 * time.Second
)
