// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

// TestClientSendsEpochAndRejectsAnOldDaemon pins both halves of the client
// transport: every request carries Rafiki-Protocol, and a response whose header
// is missing (an old daemon) or different is refused with a message naming both
// epochs — never silently accepted.
func TestClientSendsEpochAndRejectsAnOldDaemon(t *testing.T) {
	c := assert.NewAborting(t)

	var gotReq string
	oldDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Header.Get(protocol.EpochHeader)
		w.WriteHeader(http.StatusOK) // no Rafiki-Protocol on the response
	}))
	defer oldDaemon.Close()

	client := &http.Client{Transport: &bearerTransport{base: http.DefaultTransport, token: "tok"}}
	resp, err := client.Get(oldDaemon.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	c.Eq(strconv.Itoa(protocol.Epoch), gotReq, "every request must carry the epoch")
	c.Error(err, "a daemon that omits the response epoch must be rejected")
	c.StrContains(err.Error(),
		"daemon speaks rafiki protocol none; this client speaks 2 — upgrade the daemon",
		"old-daemon error")

	futureDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(protocol.EpochHeader, "3")
		w.WriteHeader(http.StatusOK)
	}))
	defer futureDaemon.Close()

	resp, err = client.Get(futureDaemon.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	c.Error(err, "a daemon speaking epoch 3 must be rejected")
	c.StrContains(err.Error(),
		"daemon speaks rafiki protocol 3; this client speaks 2 — upgrade the daemon",
		"wrong-epoch error")
}

// TestClientDoesNotMaskAProtocolMismatchError pins the one exception to the
// response-header check: when the daemon itself refuses with a
// protocol_mismatch Connect error (even without the response header), the
// client must surface THAT error, not replace it with its own
// "upgrade the daemon" text.
func TestClientDoesNotMaskAProtocolMismatchError(t *testing.T) {
	c := assert.NewAborting(t)
	// A real Connect error, built the way the daemon's own gate builds it — but
	// answered WITHOUT the Rafiki-Protocol response header, the shape the client
	// must not mask.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := rpcreason.Attach(connect.NewError(connect.CodeFailedPrecondition,
			errors.New("this daemon speaks rafiki protocol 2; upgrade your rafiki client (peer sent none)")),
			protocol.ErrProtocolMismatch)
		_ = connect.NewErrorWriter().Write(w, r, err)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &bearerTransport{base: http.DefaultTransport}}
	ctrl := rafikiv1connect.NewControlClient(client, srv.URL)
	_, err := ctrl.ListModels(context.Background(), connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	c.Error(err, "expected the daemon's refusal")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code must be the daemon's, not a transport error")
	c.Eq(protocol.ErrProtocolMismatch, rpcreason.Reason(err), "the daemon's reason must survive")
	c.NotStrContains(err.Error(), "upgrade the daemon", "the client's own epoch error must not replace the daemon's")
}

// TestEpochClientDoesNotMaskAProtocolMismatchError is the -run 'Epoch'-visible
// entry for the test above, whose own name does not contain "Epoch".
func TestEpochClientDoesNotMaskAProtocolMismatchError(t *testing.T) {
	t.Run("ClientDoesNotMaskAProtocolMismatchError", TestClientDoesNotMaskAProtocolMismatchError)
}

// epochResponseHeader makes a fake daemon answer with the current epoch, the
// way the real daemon's RequireEpoch gate does, so the CLI's client transport
// (which verifies the response header) accepts it.
func epochResponseHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(protocol.EpochHeader, strconv.Itoa(protocol.Epoch))
		next.ServeHTTP(w, r)
	})
}
