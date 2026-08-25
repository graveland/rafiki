// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/paths"
)

// connectUDSBaseURL is a sentinel. Over a unix socket the host is meaningless
// — the dialer decides the destination — but net/http still needs a
// syntactically valid absolute URL. ".invalid" is reserved by RFC 2606 and can
// never resolve, so a misconfiguration that bypasses the dialer fails loudly
// rather than reaching a real host.
const connectUDSBaseURL = "http://connect.rafiki.invalid"

// connectHTTPClient speaks h2c over the given unix socket. It must match
// cmd/rafikid/connect_uds.go, which serves h2c on the same socket.
func connectHTTPClient(socketPath string) *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}}
}

// newControlClient returns a Connect client for the local daemon.
func newControlClient(cmd *cobra.Command) (rafikiv1connect.ControlClient, error) {
	sock := paths.ConnectSocketPath()
	return rafikiv1connect.NewControlClient(connectHTTPClient(sock), connectUDSBaseURL), nil
}

// diagnoseConnectError turns a Connect failure into something that names the
// cause.
//
// The motivating failure: `rafiki tui <id>` against a daemon with no Connect
// routes produced "unimplemented: 404 Not Found", which reads like a missing
// RPC. net/http answers an unrouted path with a bodiless 404 page, and Connect
// maps a bodiless 404 to CodeUnimplemented. There is exactly one cause now
// that the daemon requires a database: the daemon is older than the Connect
// control plane.
func diagnoseConnectError(err error, socketPath string) error {
	if err == nil {
		return nil
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnimplemented:
		return fmt.Errorf(
			"this rafikid predates the Connect control plane the TUI needs "+
				"(the daemon answered an unrouted path at %s). "+
				"Rebuild and reinstall rafikid from this tree, then restart it: %w",
			socketPath, err)
	case connect.CodeUnavailable:
		return fmt.Errorf(
			"cannot reach the rafiki daemon at %s — is rafikid running? (`rafiki status`): %w",
			socketPath, err)
	default:
		return err
	}
}
