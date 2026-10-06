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

// TestClientEpochRefusalIsTerminalFailedPrecondition drives the REAL generated
// Connect client through bearerTransport against a daemon that omits the
// response epoch (an old daemon). The refusal must be a Connect
// failed_precondition carrying the protocol_mismatch reason — not a plain
// error connect wraps as Unavailable, which would render as "is rafikid
// running?" and make an Unavailable-retrying caller (the cockpit's stream loop
// included) retry an old daemon forever.
func TestClientEpochRefusalIsTerminalFailedPrecondition(t *testing.T) {
	c := assert.NewAborting(t)
	oldDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // no Rafiki-Protocol: an epoch-1 daemon
	}))
	defer oldDaemon.Close()

	client := &http.Client{Transport: &bearerTransport{base: http.DefaultTransport}}
	ctrl := rafikiv1connect.NewControlClient(client, oldDaemon.URL)

	_, err := ctrl.ListModels(context.Background(), connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	c.Error(err, "an old daemon must be refused")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err),
		"unary: the client's epoch refusal must be failed_precondition, never unavailable")
	c.Eq(protocol.ErrProtocolMismatch, rpcreason.Reason(err),
		"unary: the protocol_mismatch reason must survive")

	stream, err := ctrl.StreamEvents(context.Background(), connect.NewRequest(&rafikiv1.StreamEventsRequest{}))
	if err == nil {
		stream.Receive()
		err = stream.Err()
	}
	c.Error(err, "an old daemon must refuse the stream too")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err),
		"stream: terminal failed_precondition, never a retried unavailable")
	c.Eq(protocol.ErrProtocolMismatch, rpcreason.Reason(err),
		"stream: the protocol_mismatch reason must survive")

	// The CLI's rendering must show the upgrade message, not the misleading
	// "cannot reach the daemon" advice that an Unavailable produces.
	msg := connectVerbErr(err, oldDaemon.URL).Error()
	c.StrContains(msg, "protocol_mismatch", "the CLI must surface the rafiki reason")
	c.StrContains(msg, "upgrade the daemon", "the CLI must show the upgrade message")
	c.NotStrContains(msg, "is rafikid running", "an old daemon must not read as unreachable")
}

// TestEpochClientRefusalIsTerminal is the -run 'Epoch'-visible entry for the
// test above, whose own name does not contain "Epoch".
func TestEpochClientRefusalIsTerminal(t *testing.T) {
	t.Run("ClientEpochRefusalIsTerminalFailedPrecondition", TestClientEpochRefusalIsTerminalFailedPrecondition)
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
