// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/server"
)

// serveConnectUDS serves the Connect control plane to local clients over a
// unix socket, using the SAME handler the TCP/TLS listener mounts.
//
// The same handler, deliberately: this repo has twice shipped a correct guard
// that a second code path routed around. The socket changes who can reach the
// daemon, never what the daemon will accept.
//
// No token required: the interceptor is constructed with an empty token,
// which disables the ADMISSION check. Trust here is the 0600 socket inside
// the 0700 directory, matching the framed-JSON control socket — auth is who
// may connect, never gated by a credential over UDS.
//
// auth, when non-nil, still OPTIONALLY resolves identity — but now it must
// AGREE with the framed socket's optional ctrl_auth: a request with NO
// credential proceeds anonymously, one whose credential RESOLVES runs as that
// user, and one presenting a credential that does not resolve is refused —
// Unauthenticated for an unknown token, Unavailable for a store that could not
// be checked (never the store's error text) — exactly as the framed handshake
// refuses the same credential. A mount that silently downgraded a bad
// credential to anonymous while its framed sibling refused it would answer the
// same operator differently depending on which plane the request took. This
// is what lets a per-user Connect read like GetRateLimitStatus resolve "who is
// asking" over the local socket the cockpit and CLI dial by default, without
// weakening the socket's own trust-by-filesystem-permissions model for every
// other verb that never looks at identity at all.
//
// h2c rather than TLS: there is no TLS on a unix socket, and Connect's
// server-streaming (StreamEvents) wants HTTP/2. The client half is in
// cmd/rafiki/connectclient.go and must match.
func serveConnectUDS(ctx context.Context, srv *connectapi.Server, auth *server.UserTokenAuth, path string) (net.Listener, error) {
	// Refuse rather than clobber. Two daemons serving one path means the
	// second bind silently wins and the first's clients connect into a void.
	if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("%s is already served by a live daemon", path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot remove stale socket %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}

	// Umask before bind, not chmod after: chmod leaves a window in which
	// another local user can connect.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}

	mux := http.NewServeMux()
	// Empty token: the socket IS the credential for ADMISSION. The optional
	// identity interceptor rides behind it, and connectControlRoute puts the
	// policy gate innermost, behind identity resolution — the same table the
	// proxy face serves, so a child credential presented to the local socket
	// is refused on operator verbs exactly as it is remotely.
	routePath, handler := connectControlRoute(srv, connectapi.NewAuthInterceptor(""), optionalIdentityInterceptor(auth))
	mux.Handle(routePath, handler)

	// Unencrypted HTTP/2 (h2c) via the standard library's Protocols field,
	// rather than the deprecated x/net/http2/h2c wrapper. Connect's
	// server-streaming (StreamEvents) wants HTTP/2; there is no TLS on a unix
	// socket, so this is prior-knowledge h2c. The client half is in
	// cmd/rafiki/connectclient.go and must match.
	proto := &http.Protocols{}
	proto.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{
		Handler:           mux,
		Protocols:         proto,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		_ = httpSrv.Close()
	}()
	go func() {
		if err := httpSrv.Serve(ln); err != nil && ctx.Err() == nil {
			slog.Error("connect unix listener stopped", "path", path, "error", err)
		}
	}()
	return ln, nil
}

// optionalIdentityInterceptor resolves caller identity for a request that
// carries a credential, with the same three-way rule the framed unix socket
// gives its optional ctrl_auth (pkg/control's ListenWithAuth):
//
//   - no credential → proceed anonymous, untouched. The socket decided
//     admission; a credential was never the price of entry.
//   - credential resolves → the request runs as that user, which is what lets
//     a per-user Connect read like GetRateLimitStatus resolve "who is asking"
//     over the local socket the cockpit and CLI dial by default.
//   - credential present but unknown → Unauthenticated ("invalid auth token",
//     the framed handshake's wording), never a silent downgrade to anonymous:
//     the caller presented a credential expecting it to mean something, and
//     running the request under another user's (or the daemon bucket's)
//     identity would misattribute its work.
//   - identity store unavailable → Unavailable with the fixed message, never
//     the store's error text — an outage is not a bad credential, and a pgx
//     error carries the DSN.
//
// auth == nil (no identity store wired) passes everything through untouched:
// there is nothing to resolve and nothing to refuse.
func optionalIdentityInterceptor(auth *server.UserTokenAuth) connect.Interceptor {
	return optionalIdentity{auth: auth}
}

type optionalIdentity struct{ auth *server.UserTokenAuth }

// ctxFor resolves the credential carried in h and returns the context the
// handler should run with, or a Connect error to refuse the request with.
func (o optionalIdentity) ctxFor(ctx context.Context, h http.Header) (context.Context, error) {
	if o.auth == nil {
		return ctx, nil
	}
	id, err := o.auth.IdentifyStrict(ctx, h)
	if err != nil {
		if errors.Is(err, server.ErrAuthUnavailable) {
			return nil, connect.NewError(connect.CodeUnavailable,
				errors.New("identity store unavailable"))
		}
		return nil, connect.NewError(connect.CodeUnauthenticated,
			errors.New("invalid auth token"))
	}
	if id == nil {
		return ctx, nil
	}
	return server.WithIdentity(ctx, id), nil
}

func (o optionalIdentity) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := o.ctxFor(ctx, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	})
}

func (o optionalIdentity) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (o optionalIdentity) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := o.ctxFor(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	})
}
