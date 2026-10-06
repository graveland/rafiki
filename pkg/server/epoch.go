// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
)

// connectPathPrefix is the URL prefix of every Connect service the daemon
// serves — Control, Login, and any future /rafiki.v1.* service. Only these
// paths carry the epoch; the MCP face, /healthz and the executor/daraja
// upgrade routes are not Connect and are left untouched.
const connectPathPrefix = "/rafiki.v1."

// RequireEpoch is the HTTP-level protocol-epoch gate. It is applied at the
// OUTERMOST layer of every Connect mount, before authentication, because the
// Login service is mounted without authentication and must still be gated: a
// credential-less caller is exactly who Login exists to serve, but it must
// still speak the current wire.
//
// For a /rafiki.v1.* request it sets Rafiki-Protocol: 2 on the response
// unconditionally (refusals included, so a client can always read the
// daemon's epoch) and passes the request through only when its own
// Rafiki-Protocol header is exactly the daemon's epoch. A missing header is an
// epoch-1 peer; anything else is a peer this daemon cannot talk to.
//
// The refusal is a Connect error — code failed_precondition (HTTP 400),
// ErrorInfo reason protocol_mismatch — so a stale client fails with a legible
// message instead of exchanging fields the two sides no longer agree on.
func RequireEpoch(next http.Handler) http.Handler {
	want := strconv.Itoa(protocol.Epoch)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, connectPathPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set(protocol.EpochHeader, want)
		if r.Header.Get(protocol.EpochHeader) != want {
			writeEpochRefusal(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeEpochRefusal answers a request whose epoch is missing or wrong. It emits
// the form the caller's protocol expects. A Connect streaming request
// (Content-Type application/connect+) gets the enveloped HTTP-200
// end-of-stream error connect-go parses into failed_precondition plus the
// message; forcing the unary JSON form there would reach a connect-go streaming
// client as a bare "internal: HTTP status 400 Bad Request", because its
// validateResponse turns any non-200 into an error WITHOUT reading the body. A
// unary (or unknown-protocol) request keeps the connect unary JSON form — code
// failed_precondition at HTTP 400 — which every other client reads the code,
// message and reason off. Either way the code, the message and the ErrorInfo
// reason are identical.
func writeEpochRefusal(w http.ResponseWriter, r *http.Request) {
	peer := r.Header.Get(protocol.EpochHeader)
	if peer == "" {
		peer = "none"
	}
	msg := fmt.Sprintf(
		"this daemon speaks rafiki protocol %d; upgrade your rafiki client (peer sent %s)",
		protocol.Epoch, peer)
	err := rpcreason.Attach(
		connect.NewError(connect.CodeFailedPrecondition, errors.New(msg)),
		protocol.ErrProtocolMismatch)
	req := r.Clone(r.Context())
	if !isConnectStreamRequest(req) {
		req.Header.Set("Content-Type", "application/json")
	}
	_ = connect.NewErrorWriter().Write(w, req, err)
}

// isConnectStreamRequest reports whether r is a Connect streaming request, by
// the documented discriminator: a Content-Type whose media type begins with
// "application/connect+" (parameters and case irrelevant). The ErrorWriter
// classifies on the same prefix.
func isConnectStreamRequest(r *http.Request) bool {
	ctype := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = ctype[:i]
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ctype)), "application/connect+")
}
