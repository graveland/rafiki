// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// connectUDSBaseURL is a sentinel. Over a unix socket the host is meaningless
// — the dialer decides the destination — but net/http still needs a
// syntactically valid absolute URL. ".invalid" is reserved by RFC 2606 and can
// never resolve, so a misconfiguration that bypasses the dialer fails loudly
// rather than reaching a real host.
const connectUDSBaseURL = "http://connect.rafiki.invalid"

// connectHTTPClient speaks h2c over the given unix socket. It must match
// cmd/rafikid/connect_uds.go, which serves h2c on the same socket. It carries
// the protocol epoch (token-less) so even a local, credential-less profile
// speaks the current wire; newConnectEndpoint adds the bearer on top when the
// profile has one.
func connectHTTPClient(socketPath string) *http.Client {
	return &http.Client{Transport: &bearerTransport{base: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}}}
}

// bearerTransport attaches the control-plane credential AND the protocol epoch
// to every request, and verifies the epoch the daemon answers with.
//
// In the transport rather than at each call site so that the cockpit's own
// client — which this package hands to pkg/tui and never sees again — carries
// the same credential as the pre-flight calls. A per-call header would
// authenticate the pre-flight and leave the TUI's stream unauthenticated,
// which fails only once the alt screen is already up. The epoch rides the same
// transport for the same reason: a stream the TUI opens must be gated on the
// wire too, and the response-header check has to see the stream's own response
// headers, not a later message.
//
// token may be empty (a tokenless local profile, or the Login endpoint): the
// epoch is still sent, and the response is still checked.
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating: RoundTrippers must not modify the caller's
	// request.
	r := req.Clone(req.Context())
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	r.Header.Set(protocol.EpochHeader, strconv.Itoa(protocol.Epoch))
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if err := checkDaemonEpoch(resp); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// checkDaemonEpoch refuses a response whose Rafiki-Protocol header is missing
// (an old daemon) or different, naming both epochs so the operator knows which
// side to upgrade. It never masks the daemon's OWN protocol_mismatch error: if
// the body is a Connect error carrying that reason, the response is returned
// untouched so the client surfaces the daemon's more specific message.
func checkDaemonEpoch(resp *http.Response) error {
	if resp.Header.Get(protocol.EpochHeader) == strconv.Itoa(protocol.Epoch) {
		return nil
	}
	if daemonRefusedEpoch(resp) {
		return nil
	}
	got := resp.Header.Get(protocol.EpochHeader)
	if got == "" {
		got = "none"
	}
	return fmt.Errorf("daemon speaks rafiki protocol %s; this client speaks %d — upgrade the daemon",
		got, protocol.Epoch)
}

// daemonRefusedEpoch reports whether resp is a Connect error whose ErrorInfo
// reason is protocol_mismatch — the daemon's own epoch refusal. It reads (and
// restores) the body only for an error status, so a healthy response body is
// never touched.
func daemonRefusedEpoch(resp *http.Response) bool {
	if resp.StatusCode < 400 || resp.Body == nil {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return false
	}
	resp.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(data), resp.Body), Closer: resp.Body}
	var body struct {
		Details []struct {
			Debug struct {
				Reason string `json:"reason"`
			} `json:"debug"`
		} `json:"details"`
	}
	if json.Unmarshal(data, &body) != nil {
		return false
	}
	for _, d := range body.Details {
		if d.Debug.Reason == protocol.ErrProtocolMismatch {
			return true
		}
	}
	return false
}

// readCloser pairs a reconstructed reader with the original body's Closer so a
// peeked-at error body still closes the underlying connection.
type readCloser struct {
	io.Reader
	io.Closer
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

	// identity is the stable string naming this endpoint for caching: the URL
	// remotely, a "unix:"-prefixed socket path locally. The completion cache
	// keys on it, so reads, writes and drops always name the endpoint that was
	// actually resolved — one resolver, one identity, no drift.
	identity string
}

// dialAddr returns the host:port a TLS client dials for rawURL: its host:port
// if a port was given, else its host with the https default (443) appended.
// url.URL leaves an unspecified port out of Host entirely, and net.Dial
// requires one — without this, a "https://host" URL (no explicit :443) fails
// with "missing port in address" before TLS is even attempted.
//
// Inlined from the retired pkg/client.DialAddr, which existed for exactly one
// caller (the executor link, sessionConnectTarget below) precisely so this
// derivation could not drift between the control plane and the executor.
func dialAddr(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse rafiki url: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("RAFIKI_URL scheme must be 'https' to reach the control plane, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("RAFIKI_URL missing host")
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	return net.JoinHostPort(u.Hostname(), "443"), nil
}

// isRemoteURL reports whether raw names a remote rafikid worth dialing for
// the control plane. Only https does: an http:// URL is the local loopback
// proxy face — one hostname serving the face, the control plane and the
// executor link is a TLS-only arrangement, so there is no plaintext control
// listener to dial. An empty URL means the local daemon.
func isRemoteURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// newConnectEndpoint resolves where the Connect control plane lives, from the
// profile and nothing else.
//
// The profile's Socket IS the Connect socket: the daemon serves its control
// plane on paths.SocketPath, the one path profiles store. It is deliberately
// not a second profile field: two names for one daemon is two ways to be
// wrong.
//
// Remote requires a token. There is no bootstrap mode on this plane — a
// daemon with no users refuses every connection — so an absent credential can
// only ever produce an Unauthenticated, and saying so here beats saying so
// after a round trip.
func newConnectEndpoint(cmd *cobra.Command) (connectEndpoint, error) {
	p, err := resolveProfile(cmd)
	if err != nil {
		return connectEndpoint{}, err
	}

	if p.URL == "" {
		sock := p.Socket
		httpClient := connectHTTPClient(sock)
		// Attach the profile's token when it has one, so a per-user read
		// (GetRateLimitStatus) can resolve identity locally too — see
		// UserTokenAuth.IdentifyOptional. Optional, unlike the remote branch:
		// the socket itself remains the trust boundary, connect_uds.go builds
		// its interceptor with an empty token, and a local profile with no
		// token must keep working for every verb that never looks at identity.
		if p.Token != "" {
			httpClient = &http.Client{Transport: &bearerTransport{base: httpClient.Transport, token: p.Token}}
		}
		return connectEndpoint{
			httpClient: httpClient,
			baseURL:    connectUDSBaseURL,
			describe:   sock,
			identity:   "unix:" + sock,
		}, nil
	}

	if p.Token == "" {
		return connectEndpoint{}, fmt.Errorf(
			"profile %q names a remote daemon but has no token: write one to %s "+
				"(or recreate it with `rafiki profile add %s --url %s --token …`)",
			p.Name, profile.TokenFile(p.Name), p.Name, p.URL)
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
			token: p.Token,
		}},
		baseURL:  p.URL,
		describe: p.URL,
		identity: p.URL,
	}, nil
}

// newLoginEndpoint resolves where the Login service lives: the same profile
// resolution as newConnectEndpoint, minus the credential.
//
// The difference is deliberate, not sloppy. The Login service is served
// OUTSIDE authentication on both planes (the UDS and the remote https URL) —
// it exists to mint a credential for a caller that has none — so:
//
//   - a tokenless REMOTE profile is the normal state of a machine that has not
//     logged in yet, and is accepted here (newConnectEndpoint refuses it,
//     because every other verb needs the credential);
//   - a token that IS present is never attached: an expired credential riding
//     the login round trip could only confuse the one service that must not
//     care about credentials.
func newLoginEndpoint(cmd *cobra.Command) (connectEndpoint, error) {
	p, err := resolveProfile(cmd)
	if err != nil {
		return connectEndpoint{}, err
	}

	if p.URL == "" {
		sock := p.Socket
		return connectEndpoint{
			httpClient: connectHTTPClient(sock),
			baseURL:    connectUDSBaseURL,
			describe:   sock,
			identity:   "unix:" + sock,
		}, nil
	}

	return connectEndpoint{
		httpClient: &http.Client{Transport: &bearerTransport{base: http.DefaultTransport}},
		baseURL:    p.URL,
		describe:   p.URL,
		identity:   p.URL,
	}, nil
}

// control returns a Connect client for the endpoint.
func (e connectEndpoint) control() rafikiv1connect.ControlClient {
	return rafikiv1connect.NewControlClient(e.httpClient, e.baseURL)
}

// login returns a Login client for the endpoint. The Login service rides the
// same transports as the control plane — h2c on the profile's socket, https on
// a remote URL — but answers tokenless callers, so the endpoint behind it is
// newLoginEndpoint's.
func (e connectEndpoint) login() rafikiv1connect.LoginClient {
	return rafikiv1connect.NewLoginClient(e.httpClient, e.baseURL)
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
			"the rafiki daemon at %s rejected the profile's credential: your token is invalid or expired; run 'rafiki login' to mint a new one, or check its token file: %w",
			endpoint, err)
	case connect.CodeUnavailable:
		return fmt.Errorf(
			"cannot reach the rafiki daemon at %s — is rafikid running? (`rafiki status`): %w",
			endpoint, err)
	default:
		return err
	}
}
