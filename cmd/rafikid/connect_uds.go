// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
)

// serveConnectUDS serves the Connect control plane to local clients over a
// unix socket, using the SAME handler the TCP/TLS listener mounts.
//
// The same handler, deliberately: this repo has twice shipped a correct guard
// that a second code path routed around. The socket changes who can reach the
// daemon, never what the daemon will accept.
//
// No token: the interceptor is constructed with an empty token, which disables
// the check. Trust here is the 0600 socket inside the 0700 directory, matching
// the framed-JSON control socket.
//
// h2c rather than TLS: there is no TLS on a unix socket, and Connect's
// server-streaming (StreamEvents) wants HTTP/2. The client half is in
// cmd/rafiki/connectclient.go and must match.
func serveConnectUDS(ctx context.Context, srv *connectapi.Server, path string) (net.Listener, error) {
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
	// Empty token: the socket IS the credential.
	mux.Handle(srv.Routes(connectapi.NewAuthInterceptor("")))

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
