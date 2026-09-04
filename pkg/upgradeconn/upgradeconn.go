// Package upgradeconn turns an HTTP/1.1 request into a raw connection, so
// several byte-stream protocols can share one TLS listener and be routed by
// PATH like ordinary HTTP.
//
// Why this exists. rafiki has three network surfaces, and only one of them is
// an ordinary HTTP server:
//
//   - the proxy face — plain HTTP, a mux, paths;
//   - the control plane — newline-delimited JSON frames, no paths at all;
//   - the executor link — the executor DIALS in and then SERVES HTTP/2 on the
//     connection it dialled, so rafikid is the HTTP *client* there and never
//     receives a request it could route.
//
// The last one is the interesting case: it is not that the executor fails to
// connect to us, it is that once connected the request direction reverses. A
// mux answers requests; on that socket rafikid is the one asking. So neither
// the control plane nor the executor link can be path-routed as they stand.
//
// Putting an HTTP request IN FRONT of the stream fixes both. The client sends
// an ordinary `GET /path` with an Upgrade header, the server's mux routes it by
// path like anything else, the handler hijacks the connection and replies 101,
// and only then does the byte-stream protocol begin. Same trick as WebSocket.
//
// The payoff is one port, one certificate, one ingress rule — and, because an
// Upgrade tunnel is what every HTTP proxy already understands, the option of
// letting an ingress terminate TLS instead of requiring passthrough.
package upgradeconn

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Conn is a hijacked connection that reads through the buffer the HTTP server
// left behind.
//
// This wrapper is the whole reason the two protocols underneath stay correct,
// and both of their framing rules are otherwise landmines:
//
//   - The control plane must not lose a request the client PIPELINED right
//     behind its auth frame. Clients routinely send both in one segment, so
//     those bytes are sitting in the hijack buffer, not on the socket. Reading
//     the raw net.Conn would silently drop them and hang the client to its
//     timeout.
//   - The executor link must not have its HTTP/2 client preface swallowed. It
//     sends a hello frame and then immediately starts speaking h2, so a reader
//     that buffers past the newline and is then discarded takes the preface
//     with it.
//
// Those pull in opposite directions — one says "keep the buffer", the other
// says "do not over-read" — which is why the codebase documents them as
// opposite rules in two places. Reading EVERYTHING through one wrapper
// dissolves the conflict: over-reading is harmless because nothing is
// discarded, and nothing is lost because there is only ever one reader.
//
// The rule this replaces them with is simpler and harder to get wrong: after
// Upgrade, never read the underlying net.Conn directly. Use this.
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
	// Control is rafiki's framed JSON control plane.
	Control Protocol = "rafiki-control"
	// Executor is the reverse-dialled executor link: a hello frame, then
	// HTTP/2 with the roles inverted.
	Executor Protocol = "rafiki-executor"
	// Daraja is the reverse-dialled per-child host link: a hello frame, then
	// HTTP/2 with the roles inverted, exactly as Executor. Its own path because
	// the two carry different hello frames and reach different registries.
	Daraja Protocol = "rafiki-daraja"
)

// Handler returns an http.Handler that upgrades a matching request and hands
// the resulting connection to serve.
//
// serve owns the connection and must close it. It runs on the request's
// goroutine, which the http.Server no longer tracks once hijacked, so a handler
// that blocks forever leaks exactly one goroutine and one connection — the same
// bargain any long-lived accept loop makes.
func Handler(proto Protocol, serve func(*Conn)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), string(proto)) {
			http.Error(w, fmt.Sprintf("this endpoint speaks %s; send Upgrade: %s", proto, proto),
				http.StatusUpgradeRequired)
			return
		}

		hj, ok := w.(http.Hijacker)
		if !ok {
			// net/http implements Hijacker on HTTP/1.x only — there is no
			// hijack on HTTP/2. The listener in front of this must therefore
			// serve 1.1, which is why it does not enable h2.
			http.Error(w, "server does not support connection upgrade", http.StatusInternalServerError)
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
			"Connection: Upgrade\r\n\r\n"); err != nil {
			conn.Close()
			return
		}
		if err := brw.Flush(); err != nil {
			conn.Close()
			return
		}

		// brw.Reader, not conn: see the Conn doc comment.
		serve(&Conn{Conn: conn, r: brw.Reader})
	})
}

// Dial performs the client half: it sends the upgrade request on an
// already-established connection and consumes the 101 response, returning a
// Conn positioned at the first byte of the upgraded protocol.
//
// The response is read with a bufio.Reader that is then CARRIED FORWARD in the
// returned Conn. A server that pipelines its first bytes behind the 101 — which
// the executor link does not, but a future protocol might — would otherwise
// have them read into a buffer that is thrown away.
func Dial(conn net.Conn, proto Protocol, host string) (*Conn, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+host+PathFor(proto), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Upgrade", string(proto))
	req.Header.Set("Connection", "Upgrade")

	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("upgrade: write request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("upgrade: read response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("upgrade: server answered %s, want 101 Switching Protocols", resp.Status)
	}
	return &Conn{Conn: conn, r: br}, nil
}

// PathFor is the single source of truth for each protocol's path, so the dialler
// and the mux cannot disagree.
func PathFor(proto Protocol) string {
	switch proto {
	case Control:
		return "/control"
	case Executor:
		return "/executor/connect"
	case Daraja:
		return "/daraja/connect"
	default:
		return "/"
	}
}
