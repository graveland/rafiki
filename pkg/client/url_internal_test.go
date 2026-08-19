package client

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
	"go.graveland.dev/rafiki/pkg/users"
)

// zeroUsersAuthenticator reports no active users, which puts
// control.authHandshake into bootstrap mode: a first frame that is NOT
// ctrl_auth is admitted and handed back to be dispatched as the request.
type zeroUsersAuthenticator struct{}

func (zeroUsersAuthenticator) Authenticate(_ context.Context, _ string) (users.Identity, error) {
	return users.Identity{}, users.ErrNotFound
}

func (zeroUsersAuthenticator) CountActive(context.Context) (int, error) { return 0, nil }

// parseControlURL is unexported, so its cases live here (package client)
// rather than in client_test.go, which is package client_test.

func TestParseControlURLRequiresHTTPS(t *testing.T) {
	if _, err := parseControlURL("https://host:443"); err != nil {
		t.Fatalf("https rejected: %v", err)
	}
	if _, err := parseControlURL("tls://host:443"); err == nil {
		t.Fatal("the retired tls:// scheme was accepted")
	}
	if _, err := parseControlURL("http://host:8035"); err == nil {
		t.Fatal("http:// was accepted for the control plane; there is no plaintext listener")
	}
	if _, err := parseControlURL("https://"); err == nil {
		t.Fatal("missing host was accepted")
	}
}

// TestDialAddr covers the port defaulting DialURL relies on: url.URL leaves
// an unspecified port out of u.Host, and net.Dial rejects a bare hostname
// with "missing port in address" — the failure a plain
// RAFIKI_URL=https://host (no :443) used to hit before dialAddr existed.
func TestDialAddr(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"https://host", "host:443"},
		{"https://host:443", "host:443"},
		{"https://host:8443", "host:8443"},
		{"https://[::1]", "[::1]:443"},
		{"https://[::1]:9443", "[::1]:9443"},
	} {
		u, err := parseControlURL(tc.raw)
		if err != nil {
			t.Fatalf("parseControlURL(%q): %v", tc.raw, err)
		}
		if got := dialAddr(u); got != tc.want {
			t.Errorf("dialAddr(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// sendAuthFrame is the exact code DialURL calls once the TLS dial and
// upgrade have succeeded — testing it directly here sidesteps the fact that
// DialURL itself only ever trusts system root CAs, so a self-signed test
// server can never get far enough through TLS verification to observe what
// DialURL wrote on the wire.

// TestSendAuthFrame_EmptyTokenWritesNothing is the no-token dial path: DialURL
// must skip the ctrl_auth frame entirely so a bootstrap daemon's first frame
// is the caller's real request, not an auth handshake it would reject.
func TestSendAuthFrame_EmptyTokenWritesNothing(t *testing.T) {
	server, cli := net.Pipe()
	defer server.Close()
	defer cli.Close()

	// No goroutine reading server: if sendAuthFrame wrote anything, the
	// write would block on the unbuffered pipe. A deadline turns that into
	// a legible failure (deadline exceeded) instead of hanging to the test
	// binary's own timeout with no indication of which assertion failed.
	if err := cli.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set write deadline: %v", err)
	}
	if err := sendAuthFrame(cli, ""); err != nil {
		t.Fatalf("sendAuthFrame with empty token returned an error: %v", err)
	}
}

// TestSendAuthFrame_TokenPresentWritesCtrlAuth is the existing (non-bootstrap)
// path: a present token must still produce exactly the ctrl_auth frame the
// daemon's authHandshake expects.
func TestSendAuthFrame_TokenPresentWritesCtrlAuth(t *testing.T) {
	server, cli := net.Pipe()
	defer server.Close()
	defer cli.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- sendAuthFrame(cli, "secret-token") }()

	fr := protocol.NewFrameReader(server, protocol.MaxFrameBytes)
	frame, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("sendAuthFrame: %v", err)
	}

	var req protocol.AuthRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		t.Fatalf("unmarshal auth frame: %v", err)
	}
	if req.Type != protocol.TypeCtrlAuth {
		t.Errorf("type = %q, want %q", req.Type, protocol.TypeCtrlAuth)
	}
	if req.Token != "secret-token" {
		t.Errorf("token = %q, want %q", req.Token, "secret-token")
	}
}

// TestReadLoop_UnmatchedAuthFailureIsPinnedAsTheCloseReason is M1: an
// auth-failure ctrl_response with no waiter (id "0", which no real Request
// ever assigns) must not be silently dropped. Both no-token-non-bootstrap
// (auth_required) and wrong-token (auth_invalid) produce exactly this shape
// — the server answers id "0" and closes, while the client's real Request
// is waiting on its own, different id. Before this fix the caller only ever
// saw errClosedConn(nil) ("client connection closed"), with the actual
// reason silently dropped in readLoop's response-routing switch.
func TestReadLoop_UnmatchedAuthFailureIsPinnedAsTheCloseReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{"no token, not bootstrap", protocol.ErrAuthRequired},
		{"wrong token", protocol.ErrAuthInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			defer serverConn.Close()
			defer clientConn.Close()

			c := &Client{
				conn:    clientConn,
				closeCh: make(chan struct{}),
				subs:    make(map[uint64]chan []byte),
			}
			go c.readLoop()

			// The "server": read whatever the client sends first (the real
			// request — no ctrl_auth precedes it in the no-token case;
			// wrong-token's own ctrl_auth already went out via sendAuthFrame
			// before this Client was even constructed, mirroring DialURL),
			// answer id "0" with the failure, then hang up.
			go func() {
				fr := protocol.NewFrameReader(serverConn, protocol.MaxFrameBytes)
				_, _ = fr.ReadFrame()
				resp := protocol.Response{
					Type:    protocol.TypeCtrlResponse,
					Command: protocol.TypeCtrlAuth,
					ID:      "0",
					Success: false,
					Error:   &protocol.ErrorBody{Code: tc.code, Message: "denied"},
				}
				b, _ := json.Marshal(resp)
				_ = protocol.WriteFrame(serverConn, b)
				serverConn.Close()
			}()

			_, err := c.Request(context.Background(), map[string]any{"type": "ctrl_user_create"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Errorf("error = %q, want it to contain %q (the real close reason, not a bare \"client connection closed\")",
					err.Error(), tc.code)
			}
		})
	}
}

// TestServeConn_CallsSendAuthFrameWithTheGivenToken is M2: DialURL's real
// tail (serveConn) must be the thing exercised, not just sendAuthFrame in
// isolation — otherwise nothing pins DialURL's call site to the helper, and
// a typo there (a stray transform on token, a swapped argument) would leave
// every other test green. This drives serveConn directly over a net.Pipe,
// which is what makes it reachable at all: DialURL only ever trusts system
// root CAs, so no self-signed test server can get this far through it.
func TestServeConn_CallsSendAuthFrameWithTheGivenToken(t *testing.T) {
	t.Run("token present: ctrl_auth goes out with that token", func(t *testing.T) {
		serverConn, clientConn := net.Pipe()
		defer serverConn.Close()

		frameCh := make(chan []byte, 1)
		go func() {
			_ = serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
			fr := protocol.NewFrameReader(serverConn, protocol.MaxFrameBytes)
			frame, err := fr.ReadFrame()
			if err != nil {
				close(frameCh)
				return
			}
			frameCh <- frame
		}()

		c, err := serveConn(clientConn, "the-real-token")
		if err != nil {
			t.Fatalf("serveConn: %v", err)
		}
		defer c.Close()

		frame, ok := <-frameCh
		if !ok {
			t.Fatal("expected a ctrl_auth frame, got none (read timed out)")
		}
		var req protocol.AuthRequest
		if err := json.Unmarshal(frame, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if req.Type != protocol.TypeCtrlAuth {
			t.Errorf("type = %q, want %q", req.Type, protocol.TypeCtrlAuth)
		}
		if req.Token != "the-real-token" {
			t.Errorf("token on the wire = %q, want %q", req.Token, "the-real-token")
		}
	})

	t.Run("no token: nothing goes out", func(t *testing.T) {
		serverConn, clientConn := net.Pipe()
		defer serverConn.Close()

		frameCh := make(chan []byte, 1)
		go func() {
			_ = serverConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			fr := protocol.NewFrameReader(serverConn, protocol.MaxFrameBytes)
			frame, err := fr.ReadFrame()
			if err != nil {
				close(frameCh)
				return
			}
			frameCh <- frame
		}()

		c, err := serveConn(clientConn, "")
		if err != nil {
			t.Fatalf("serveConn: %v", err)
		}
		defer c.Close()

		if frame, ok := <-frameCh; ok {
			t.Errorf("expected no frame with an empty token, got: %s", frame)
		}
	})
}

// TestServeConn_NoToken_OverTheShippedUpgradePath is L5: the other bootstrap
// tests in this package dial control.ListenTCP directly, which nothing in
// production calls — cmd/rafikid/main.go wires control.Server.ServeUpgraded
// behind an HTTP Upgrade at /control on the shared TLS listener
// (pkg/upgradeconn), and ListenTCP is a second, TCP-direct entry point the
// daemon never uses. This test goes through the actual shipped shape
// instead: a real TCP listener, upgradeconn.Handler in front of
// control.NewAttached/ServeUpgraded, and upgradeconn.Dial on the client
// side. TLS is omitted (upgradeconn is transport-agnostic; it doesn't touch
// what TLS would add), but the HTTP Upgrade handshake and the hijack-buffer
// plumbing are exactly what production runs.
//
// The no-token path is where this interaction is riskiest: with no
// ctrl_auth frame, nothing at all is written between the upgrade's 101
// response and the caller's first real frame, so there is no pipelined data
// for upgradeconn's buffered reader to preserve — this proves the ABSENCE
// of a frame doesn't confuse it either.
func TestServeConn_NoToken_OverTheShippedUpgradePath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := control.NewAttached(control.FuncHandler(func(conn control.Connection, frame []byte) []byte {
		var hdr struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		_ = json.Unmarshal(frame, &hdr)
		resp := protocol.Response{
			Type:    protocol.TypeCtrlResponse,
			Command: hdr.Type,
			ID:      hdr.ID,
			Success: true,
		}
		if conn.Restricted() {
			resp.Data = json.RawMessage(`"restricted"`)
		}
		b, _ := json.Marshal(resp)
		return b
	}))
	defer srv.Close()

	mux := http.NewServeMux()
	mux.Handle(upgradeconn.PathFor(upgradeconn.Control), upgradeconn.Handler(upgradeconn.Control, func(c *upgradeconn.Conn) {
		srv.ServeUpgraded(c, zeroUsersAuthenticator{})
	}))
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	upConn, err := upgradeconn.Dial(raw, upgradeconn.Control, ln.Addr().String())
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	c, err := serveConn(upConn, "") // the no-token dial path
	if err != nil {
		t.Fatalf("serveConn: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Request(ctx, map[string]any{"type": "ctrl_user_create"})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Success = false, want true (bootstrap should admit this)")
	}
	if !strings.Contains(string(resp.Data), "restricted") {
		t.Errorf("data = %s, want it to show the connection arrived Restricted() (bootstrap-admitted)", resp.Data)
	}
}

// DialURL's upgrade deadline rests on one assumption: that a deadline set on
// the connection actually bounds upgradeconn.Dial's read of the response head.
// If it did not, a daemon that accepts the connection and then goes silent
// would hang the dial forever — the server's own handshake timeout is no help
// when the server is the thing that is stuck.
//
// Exercised over plain TCP rather than through DialURL, which only ever trusts
// system root CAs and so cannot be pointed at a test server.
func TestUpgradeReadIsBoundedByTheConnDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c // accept, then deliberately never respond
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	start := time.Now()
	_, err = upgradeconn.Dial(conn, upgradeconn.Control, ln.Addr().String())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("upgrade against a silent server succeeded")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("upgrade took %v to give up; the deadline is not bounding the read", elapsed)
	}
	select {
	case c := <-accepted:
		c.Close()
	default:
	}
}
