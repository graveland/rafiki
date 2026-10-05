// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// epochRoundTripper makes a test client speak the current wire: every request
// carries Rafiki-Protocol. It mirrors cmd/rafiki's client transport, which the
// daemon's gate now requires.
type epochRoundTripper struct{ base http.RoundTripper }

func (t epochRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(protocol.EpochHeader, strconv.Itoa(protocol.Epoch))
	return t.base.RoundTrip(r)
}

// epochClient wraps c so every request it makes carries the epoch header.
func epochClient(c *http.Client) *http.Client {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{Transport: epochRoundTripper{base: base}}
}

// rawUDSHTTPClient dials the unix socket speaking h2c but WITHOUT the epoch —
// the shape of a stale client, used to prove the gate refuses it.
func rawUDSHTTPClient(sock string) *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

// TestLoginIsEpochGated pins that the epoch gate covers the Login route on the
// UDS mount even though Login is mounted without authentication: a caller with
// no epoch is refused as a protocol mismatch, and one that carries it reaches
// BeginLogin (answering its own unconfigured error, never a gate refusal).
func TestLoginIsEpochGated(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := connectapi.NewServer(nil)
	auth := server.NewUserTokenAuth(nil, "unused-child-secret", time.Minute)
	sock := filepath.Join(shortTempDir(t), "s")
	ln, err := serveConnectUDS(ctx, srv, auth, newStreamRegistry(), sock)
	c.NoError(err, "serveConnectUDS")
	defer ln.Close()

	client := rafikiv1connect.NewLoginClient(rawUDSHTTPClient(sock), "http://connect.rafiki.invalid")

	// No epoch header: refused at the gate, before Login's own handling.
	req := connect.NewRequest(&rafikiv1.BeginLoginRequest{RedirectPort: 8080, ClientHost: "laptop"})
	req.Header().Set("Authorization", "Bearer definitely-not-a-token")
	_, err = client.BeginLogin(ctx, req)
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code without the epoch")
	c.Eq(protocol.ErrProtocolMismatch, rpcreason.Reason(err), "reason without the epoch")
	c.StrContains(err.Error(), "this daemon speaks rafiki protocol 2", "message without the epoch")

	// With the epoch, the credential-less caller reaches BeginLogin and gets
	// Login's own answer (OIDC unconfigured), proving the route is live behind
	// the gate.
	req2 := connect.NewRequest(&rafikiv1.BeginLoginRequest{RedirectPort: 8080, ClientHost: "laptop"})
	req2.Header().Set("Authorization", "Bearer definitely-not-a-token")
	req2.Header().Set(protocol.EpochHeader, strconv.Itoa(protocol.Epoch))
	_, err = client.BeginLogin(ctx, req2)
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code with the epoch")
	c.Eq("", rpcreason.Reason(err), "the gate did not refuse; reason")
	c.StrContains(err.Error(), "OIDC login is not configured", "message with the epoch")
}
