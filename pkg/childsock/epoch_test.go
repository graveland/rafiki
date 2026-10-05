// SPDX-License-Identifier: Apache-2.0

package childsock

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestChildSocketForwardsTheEpochHeader pins that the per-child proxy does not
// strip Rafiki-Protocol: the daemon's gate sits behind this socket, so the
// child's own epoch must reach it untouched.
func TestChildSocketForwardsTheEpochHeader(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	var seen string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(protocol.EpochHeader)
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := ServeHandler(ctx, tempSocketDir(t), inner, "child-secret-1")
	c.NoError(err, "serve")
	t.Cleanup(func() { _ = srv.Close() })

	resp := dial(t, srv.Path(), func(req *http.Request) {
		req.Header.Set(protocol.EpochHeader, strconv.Itoa(protocol.Epoch))
	}, "/rafiki.v1.Control/ListModels")
	defer resp.Body.Close()
	c.Eq(http.StatusOK, resp.StatusCode, "proxy status")
	c.Eq(strconv.Itoa(protocol.Epoch), seen, "the child's epoch must reach the daemon")
}

// TestChildSocketSynthesizedErrorCarriesTheEpoch pins that the proxy's own
// Connect error body (code unavailable, when the daemon is unreachable) still
// carries the epoch response header — a client's response-header check reads
// this proxy as current-wire, never as an old daemon.
func TestChildSocketSynthesizedErrorCarriesTheEpoch(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	// A target nothing listens on: the proxy's transport fails and its
	// ErrorHandler answers the synthesized 503.
	target, err := url.Parse("http://127.0.0.1:1")
	c.NoError(err, "parse target")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := ServeTransport(ctx, tempSocketDir(t), target, "child-secret-1", nil)
	c.NoError(err, "serve")
	t.Cleanup(func() { _ = srv.Close() })

	resp := dial(t, srv.Path(), nil, "/rafiki.v1.Control/ListModels")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	c.Eq(http.StatusServiceUnavailable, resp.StatusCode, "proxy status")
	c.Eq(strconv.Itoa(protocol.Epoch), resp.Header.Get(protocol.EpochHeader),
		"the synthesized error must carry the epoch")
	c.StrContains(string(body), "unavailable", "the body must still name the Connect code")
}
