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

// ALPNProtocols is what BOTH sides must advertise. If the executor — the TLS
// CLIENT in this arrangement — omits it, ALPN resolves to "" and ServeConn
// still works, so the negotiation guarantee is lost silently rather than
// loudly. Exported so the executor side cannot spell it differently.
var ALPNProtocols = []string{"h2"}

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
	}
	return &http.Client{Transport: tr}, nil
}
