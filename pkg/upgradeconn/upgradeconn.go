// SPDX-License-Identifier: Apache-2.0

// Package upgradeconn turns an HTTP/1.1 request into a raw connection, so
// several byte-stream protocols can share one TLS listener and be routed by
// PATH like ordinary HTTP.
//
// Why this exists. rafiki has two network surfaces that share one TLS
// listener, and only one of them is an ordinary HTTP server:
//
//   - the proxy face — plain HTTP, a mux, paths;
//   - the executor link — the executor DIALS in and then SERVES HTTP/2 on the
//     connection it dialled, so rafikid is the HTTP *client* there and never
//     receives a request it could route.
//
// The second one is the interesting case: it is not that the executor fails
// to connect to us, it is that once connected the request direction reverses.
// A mux answers requests; on that socket rafikid is the one asking. So the
// executor link cannot be path-routed as it stands.
//
// Putting an HTTP request IN FRONT of the stream fixes it. The client sends
// an ordinary `GET /path` with an Upgrade header, the server's mux routes it
// by path like anything else, and the handler authenticates the request —
// `Authorization: Bearer|Enroll|Ticket <secret>` — BEFORE hijacking, so a
// refusal is an ordinary HTTP status, no connection is ever hijacked, and no
// byte-stream protocol begins. Credentials the server mints during that
// exchange ride the 101 response's headers (Rafiki-Credential,
// Rafiki-Executor-Id). Once the 101 is written the byte-stream protocol
// begins: HTTP/2, with the roles inverted, immediately. Same trick as
// WebSocket, with the authentication moved onto the request.
//
// The payoff is one port, one certificate, one ingress rule — and, because an
// Upgrade tunnel is what every HTTP proxy already understands, the option of
// letting an ingress terminate TLS instead of requiring passthrough.
package upgradeconn

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
)

// Conn is a hijacked connection that reads through the buffer the HTTP server
// left behind.
//
// This wrapper exists because of one landmine: the upgraded link must not
// have its HTTP/2 client preface swallowed. The link begins speaking h2
// immediately after the 101, and those bytes can arrive pipelined behind the
// 101 itself — already in the hijack buffer, not on the socket — so a reader
// that over-reads and is then discarded takes the preface with it. Reading
// EVERYTHING through
// this wrapper makes over-reading harmless — nothing is discarded, and
// nothing is lost, because there is only ever one reader.
//
// The rule is simple and hard to get wrong: after Upgrade, never read the
// underlying net.Conn directly. Use this.
type Conn struct {
	net.Conn
	r *bufio.Reader
}

func (c *Conn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Reader exposes the buffered reader for a caller that needs to layer its own
// framing on top without introducing a second buffer.
func (c *Conn) Reader() *bufio.Reader { return c.r }

// Protocol names the byte-stream protocol a handler upgrades to. It is sent as
// the Upgrade header in both directions, so a mismatch is caught at the
// handshake rather than as garbage in the first frame.
type Protocol string

const (
	// Executor is the reverse-dialled executor link: after the 101, HTTP/2
	// with the roles inverted begins immediately.
	Executor Protocol = "rafiki-executor"
	// Daraja is the reverse-dialled per-child host link: after the 101,
	// HTTP/2 with the roles inverted begins immediately, exactly as Executor.
	// Its own path because the two reach different registries.
	Daraja Protocol = "rafiki-daraja"
)

// Handler returns an http.Handler that upgrades a matching request, authorizes
// it first, and hands the resulting connection to serve together with the
// authorize result.
//
// authorize runs on the request BEFORE the connection is hijacked, so a refusal
// is an ordinary HTTP response: no hijack, no goroutine, no frame parsing. It
// must not read the request body. A *Refusal error answers with its status and
// reason; any other error is logged and answered 500. The http.Header it
// returns is written on the 101, sorted for deterministic output.
//
// serve owns the connection and must close it. It runs on the request's
// goroutine, which the http.Server no longer tracks once hijacked, so a handler
// that blocks forever leaks exactly one goroutine and one connection — the same
// bargain any long-lived accept loop makes.
func Handler[T any](proto Protocol,
	authorize func(*http.Request) (T, http.Header, error),
	serve func(*Conn, T),
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), string(proto)) {
			http.Error(w, fmt.Sprintf("this endpoint speaks %s; send Upgrade: %s", proto, proto),
				http.StatusUpgradeRequired)
			return
		}

		// Checked before authorize: it has no side effects, and authorize's
		// side effects (redeeming a ticket, consuming an enroll token,
		// rotating a credential) are all one-shot — failing here after a
		// successful authorize would strand the peer with a spent credential
		// and no way to retry.
		hj, ok := w.(http.Hijacker)
		if !ok {
			// net/http implements Hijacker on HTTP/1.x only — there is no
			// hijack on HTTP/2. The listener in front of this must therefore
			// serve 1.1, which is why it does not enable h2.
			http.Error(w, "server does not support connection upgrade", http.StatusInternalServerError)
			return
		}

		t, hdr, err := authorize(r)
		if err != nil {
			var ref *Refusal
			if errors.As(err, &ref) {
				http.Error(w, ref.Reason, ref.Status)
				return
			}
			slog.Error("upgradeconn: authorize failed", "proto", proto, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		conn, brw, err := hj.Hijack()
		if err != nil {
			http.Error(w, "hijack failed", http.StatusInternalServerError)
			return
		}

		// Written by hand rather than through the ResponseWriter: it has been
		// hijacked and no longer writes anything.
		if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: " + string(proto) + "\r\n" +
			"Connection: Upgrade\r\n"); err != nil {
			conn.Close()
			return
		}
		if len(hdr) > 0 {
			keys := make([]string, 0, len(hdr))
			for k := range hdr {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				for _, v := range hdr[k] {
					if _, err := brw.WriteString(k + ": " + v + "\r\n"); err != nil {
						conn.Close()
						return
					}
				}
			}
		}
		if _, err := brw.WriteString("\r\n"); err != nil {
			conn.Close()
			return
		}
		if err := brw.Flush(); err != nil {
			conn.Close()
			return
		}

		// brw.Reader, not conn: see the Conn doc comment.
		serve(&Conn{Conn: conn, r: brw.Reader}, t)
	})
}

// Dial performs the client half: it sends the upgrade request with hdr on an
// already-established connection and consumes the 101 response, returning a
// Conn positioned at the first byte of the upgraded protocol together with the
// 101's headers. Any other status returns *Refused carrying the response body.
//
// Upgrade and Connection are set here and hdr cannot override them; everything
// else in hdr — e.g. Authorization — rides the request as given.
//
// The response is read with a bufio.Reader that is then CARRIED FORWARD in the
// returned Conn. A server that pipelines its first bytes behind the 101 — which
// the executor link does not, but a future protocol might — would otherwise
// have them read into a buffer that is thrown away.
func Dial(conn net.Conn, proto Protocol, host string, hdr http.Header) (*Conn, http.Header, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+host+PathFor(proto), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Upgrade", string(proto))
	req.Header.Set("Connection", "Upgrade")
	for k, vs := range hdr {
		// Not overridable: they identify the exchange itself.
		switch http.CanonicalHeaderKey(k) {
		case "Upgrade", "Connection":
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	if err := req.Write(conn); err != nil {
		return nil, nil, fmt.Errorf("upgrade: write request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, nil, fmt.Errorf("upgrade: read response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, nil, &Refused{Status: resp.StatusCode, Reason: strings.TrimSpace(string(body))}
	}
	return &Conn{Conn: conn, r: br}, resp.Header, nil
}

// Header names carried on the upgrade exchange.
const (
	HeaderCredential   = "Rafiki-Credential"    // 101 response: a newly minted credential
	HeaderExecutorID   = "Rafiki-Executor-Id"   // 101 response: executor row id
	HeaderChildID      = "Rafiki-Child-Id"      // request: daraja's child id
	HeaderSelfReported = "Rafiki-Self-Reported" // request: executor capability facts, url.Values-encoded
)

// Scheme is the Authorization scheme, which selects what the secret is.
type Scheme string

const (
	SchemeBearer Scheme = "Bearer" // a durable (executor) or reconnect (daraja) credential
	SchemeEnroll Scheme = "Enroll" // an executor enrollment token
	SchemeTicket Scheme = "Ticket" // a one-shot session (executor) or launch (daraja) ticket
)

// noCredentialBody is the 401 body for a missing or malformed Authorization
// header. It names the header form so a peer still speaking the JSON hello
// learns that it predates header auth and must be upgraded.
const noCredentialBody = `no credential on the upgrade request: send "Authorization: Bearer|Enroll|Ticket <secret>"; a peer sending a JSON hello frame predates header auth and must be upgraded`

// Refusal answers an upgrade with Status and Reason instead of 101.
type Refusal struct {
	Status int
	Reason string
}

func (r *Refusal) Error() string { return fmt.Sprintf("%d %s", r.Status, r.Reason) }

// Refused is what Dial returns when the server answers anything but 101.
type Refused struct {
	Status int
	Reason string // response body, trimmed, at most 4 KiB read
}

func (r *Refused) Error() string { return fmt.Sprintf("upgrade refused: %d %s", r.Status, r.Reason) }

// AuthorizationFrom parses the single Authorization header. It returns a 401
// *Refusal for a missing, repeated, schemeless, unknown-scheme, or empty-secret
// header; otherwise the canonical Scheme and the secret.
func AuthorizationFrom(r *http.Request) (Scheme, string, *Refusal) {
	vals := r.Header.Values("Authorization")
	if len(vals) != 1 {
		return "", "", &Refusal{Status: http.StatusUnauthorized, Reason: noCredentialBody}
	}
	scheme, secret, ok := strings.Cut(vals[0], " ")
	secret = strings.TrimSpace(secret)
	if !ok || secret == "" {
		return "", "", &Refusal{Status: http.StatusUnauthorized, Reason: noCredentialBody}
	}
	for _, s := range []Scheme{SchemeBearer, SchemeEnroll, SchemeTicket} {
		if strings.EqualFold(scheme, string(s)) {
			return s, secret, nil
		}
	}
	return "", "", &Refusal{Status: http.StatusUnauthorized, Reason: noCredentialBody}
}

// PathFor is the single source of truth for each protocol's path, so the dialler
// and the mux cannot disagree.
func PathFor(proto Protocol) string {
	switch proto {
	case Executor:
		return "/executor/connect"
	case Daraja:
		return "/daraja/connect"
	default:
		return "/"
	}
}
