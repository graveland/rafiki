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

// bearerTransport attaches the control-plane credential to every request.
//
// In the transport rather than at each call site so that the cockpit's own
// client — which this package hands to pkg/tui and never sees again — carries
// the same credential as the pre-flight calls. A per-call header would
// authenticate the pre-flight and leave the TUI's stream unauthenticated,
// which fails only once the alt screen is already up.
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating: RoundTrippers must not modify the caller's
	// request.
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// connectEndpoint is a resolved Connect control plane: the transport, the base
// URL that transport expects, and a human-readable name for error messages.
type connectEndpoint struct {
	httpClient *http.Client
	baseURL    string

	// describe names the endpoint in diagnostics — a socket path locally, the
	// URL remotely. Errors that say only "cannot reach the daemon" send people
	// to the wrong machine.
	describe string
}

// newConnectEndpoint resolves where the Connect control plane lives.
//
// The same gate mustDial uses (remoteDialURL — https:// only), for the same
// reason: `rafiki attach` must reach the daemon every other verb reaches. It
// used to hardcode the unix socket, so an operator with RAFIKI_URL set got a
// working `rafiki list` and a cockpit that dialed a socket on their laptop.
//
// Remote requires a token. There is no bootstrap mode on this plane — it has
// no user-create RPC — so an absent credential can only ever produce a 401,
// and saying so here beats saying so after a round trip.
func newConnectEndpoint(_ *cobra.Command) (connectEndpoint, error) {
	u := remoteDialURL()
	if u == "" {
		sock := paths.ConnectSocketPath()
		return connectEndpoint{
			httpClient: connectHTTPClient(sock),
			baseURL:    connectUDSBaseURL,
			describe:   sock,
		}, nil
	}

	token := paths.TokenFromEnv()
	if token == "" {
		return connectEndpoint{}, fmt.Errorf(
			"%s names a remote daemon but no control-plane token is set: "+
				"export %s (or write %s)", paths.URL, paths.Token, paths.TokenFile())
	}

	// Plain net/http rather than an explicit http2.Transport: the shared TLS
	// listener advertises http/1.1 only in ALPN (net/http can hijack an
	// HTTP/1.1 connection and not an HTTP/2 one, and both /control and
	// /executor/connect are Upgrades). connect-go refuses only BIDI streaming
	// below HTTP/2 — StreamEvents is server-streaming and rides HTTP/1.1
	// chunked encoding — so letting ALPN settle on http/1.1 is correct here.
	return connectEndpoint{
		httpClient: &http.Client{Transport: &bearerTransport{
			base:  http.DefaultTransport,
			token: token,
		}},
		baseURL:  u,
		describe: u,
	}, nil
}

// control returns a Connect client for the endpoint.
func (e connectEndpoint) control() rafikiv1connect.ControlClient {
	return rafikiv1connect.NewControlClient(e.httpClient, e.baseURL)
}

// diagnoseConnectError turns a Connect failure into something that names the
// cause.
//
// The motivating failure: `rafiki tui <id>` against a daemon with no Connect
// routes produced "unimplemented: 404 Not Found", which reads like a missing
// RPC. net/http answers an unrouted path with a bodiless 404 page, and Connect
// maps a bodiless 404 to CodeUnimplemented. Two causes now: a daemon older
// than the Connect control plane, or — remotely — one older than the mount
// that puts those routes on the TLS listener, where the proxy face's "/"
// answers the cockpit's path instead.
func diagnoseConnectError(err error, endpoint string) error {
	if err == nil {
		return nil
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnimplemented:
		return fmt.Errorf(
			"this rafikid predates the Connect control plane the TUI needs "+
				"(the daemon answered an unrouted path at %s). "+
				"Rebuild and reinstall rafikid from this tree, then restart it: %w",
			endpoint, err)
	case connect.CodeUnauthenticated:
		return fmt.Errorf(
			"the rafiki daemon at %s rejected the control-plane credential — "+
				"check %s (or %s): %w",
			endpoint, paths.Token, paths.TokenFile(), err)
	case connect.CodeUnavailable:
		return fmt.Errorf(
			"cannot reach the rafiki daemon at %s — is rafikid running? (`rafiki status`): %w",
			endpoint, err)
	default:
		return err
	}
}
