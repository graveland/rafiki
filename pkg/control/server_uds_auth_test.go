package control_test

// The plain UDS listener's OPTIONAL ctrl_auth handshake (ListenWithAuth).
// The TCP tests in server_tcp_test.go cover the mandatory handshake; these
// pin the UDS-specific rules: ctrl_auth is optional, the socket stays the
// local trust boundary, and a credential that cannot be honoured is refused,
// never silently downgraded to anonymous.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// udsServer starts a unix-socket control server with the given authenticator
// (nil = plain Listen) and handler. The temp dir is NOT t.TempDir(): macOS
// caps UDS paths at 104 bytes and t.TempDir embeds the test name (see
// TestServer_Broadcast in server_test.go).
func udsServer(t *testing.T, auth control.Authenticator, h control.ConnectionLifecycleHandler) (sockPath string, cleanup func()) {
	t.Helper()
	return udsServerAt(t, func(path string) (*control.Server, error) {
		if auth == nil {
			return control.Listen(path, h)
		}
		return control.ListenWithAuth(path, h, auth)
	})
}

// udsServerNoAuth is udsServer over plain Listen — no identity store wired.
func udsServerNoAuth(t *testing.T, h control.ConnectionLifecycleHandler) (string, func()) {
	t.Helper()
	return udsServer(t, nil, h)
}

func udsServerAt(t *testing.T, listen func(path string) (*control.Server, error)) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fundi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "test.sock")
	srv, err := listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	return sockPath, func() { srv.Close() }
}

// udsDial dials the socket and returns the raw conn.
func udsDial(t *testing.T, sockPath string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial %s: %v", sockPath, err)
	}
	return conn
}

// ─── the four UDS auth rules ───────────────────────────────────────────────────

// (a) ctrl_auth with a good token: the dispatched request sees that identity.
// The auth frame and the request are written in ONE call — a real client
// (client.DialWithToken) pipelines exactly like this, and a fresh FrameReader
// in handleConn would drop the pipelined request (the regression the TCP test
// TestListenTCP_PipelinedRequestAfterAuth guards).
func TestUDSAuth_GoodTokenAdmitsTheIdentity(t *testing.T) {
	seen := make(chan control.Connection, 1)
	h := control.FuncHandler(func(c control.Connection, _ []byte) []byte {
		select {
		case seen <- c:
		default:
		}
		return []byte(`{"type":"ctrl_response","command":"ctrl_status","id":"1","success":true}`)
	})
	sock, done := udsServer(t, oneUser("rfk_good", "brent"), h)
	defer done()

	conn := udsDial(t, sock)
	defer conn.Close()

	var buf bytes.Buffer
	writeFrameTo(t, &buf, protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: "rfk_good"})
	writeFrameTo(t, &buf, map[string]string{"type": protocol.TypeCtrlStatus, "id": "1"})
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp := readResponse(t, conn)
	if !resp.Success {
		t.Fatalf("request after UDS auth failed: %+v", resp.Error)
	}

	c := <-seen
	if got := c.Identity(); got.UserID != "u1" || got.Username != "brent" {
		t.Fatalf("identity = %+v, want {u1 brent}", got)
	}
	if c.Restricted() {
		t.Fatal("an authenticated UDS connection is bootstrap-restricted; UDS has no bootstrap window")
	}
}

// (b) ctrl_auth with a bad token: the same error frame the TCP handshake
// sends, then the connection is closed. Never a silent downgrade to
// anonymous.
func TestUDSAuth_InvalidTokenIsRefusedAndClosed(t *testing.T) {
	echo := control.FuncHandler(func(_ control.Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	sock, done := udsServer(t, oneUser("secret", "brent"), echo)
	defer done()

	conn := udsDial(t, sock)
	defer conn.Close()

	writeJSONFrame(t, conn, protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: "wrong"})
	resp := readResponse(t, conn)
	if resp.Success || resp.Error.Code != protocol.ErrAuthInvalid {
		t.Fatalf("resp = %+v, want %q", resp, protocol.ErrAuthInvalid)
	}
	if !strings.Contains(resp.Error.Message, "invalid auth token") {
		t.Fatalf("message = %q, want the TCP handshake's wording", resp.Error.Message)
	}

	// The refusal must not downgrade to anonymous service: the next read
	// fails because the server closed the connection.
	if _, err := readerFor(conn).ReadFrame(); err == nil {
		t.Fatal("connection stayed open after a refused ctrl_auth")
	}
}

// A store outage is "I could not check", never an invalid credential — the
// same rule the TCP handshake follows (TestAStoreOutageIsInternalNotAuthInvalid),
// and the store's error text never reaches the peer.
func TestUDSAuth_AStoreOutageIsInternalNotAuthInvalid(t *testing.T) {
	auth := &fakeAuth{err: errors.New("connection refused"), active: 1}
	echo := control.FuncHandler(func(_ control.Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	sock, done := udsServer(t, auth, echo)
	defer done()

	conn := udsDial(t, sock)
	defer conn.Close()
	writeJSONFrame(t, conn, protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: "rfk_good"})

	resp := readResponse(t, conn)
	if resp.Success || resp.Error.Code != protocol.ErrInternal {
		t.Fatalf("resp = %+v, want %q", resp, protocol.ErrInternal)
	}
	if strings.Contains(resp.Error.Message, "connection refused") {
		t.Fatalf("store error text leaked to an unauthenticated peer: %q", resp.Error.Message)
	}
}

// (c) No ctrl_auth: exactly today's behaviour — anonymous, the first frame is
// dispatched as the first request, and the connection may open and wait
// before sending anything (no TCP-style handshake deadline on UDS).
func TestUDSAuth_WithoutCtrlAuthStaysAnonymous(t *testing.T) {
	seen := make(chan control.Connection, 1)
	h := control.FuncHandler(func(c control.Connection, frame []byte) []byte {
		select {
		case seen <- c:
		default:
		}
		return append([]byte("echo:"), frame...)
	})
	sock, done := udsServer(t, oneUser("secret", "brent"), h)
	defer done()

	conn := udsDial(t, sock)
	defer conn.Close()

	// Idle before the first frame: legal on a UDS, where there is no
	// handshake deadline to violate. Long enough to catch an accidentally
	// short timeout; the TCP path's 10s one is pinned by inspection and by
	// authHandshakeTimeout's scoping to ListenTCP/ServeUpgraded.
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)

	writeFrame(t, conn, `{"type":"ctrl_status","id":"1"}`)
	if resp := readFrameString(t, conn); resp != `echo:{"type":"ctrl_status","id":"1"}` {
		t.Fatalf("first frame after a silent open: got %q, want the echo", resp)
	}

	c := <-seen
	if c.Identity().IsUser() {
		t.Fatalf("anonymous UDS connection carried identity %+v", c.Identity())
	}
	if c.Restricted() {
		t.Fatal("UDS connections are never bootstrap-restricted")
	}
}

// (d) ctrl_auth when no authenticator is configured (a daemon without
// RAFIKI_DB — plain Listen, and ListenWithAuth(_, _, nil)): refused with
// internal, not silently downgraded to anonymous.
func TestUDSAuth_CtrlAuthWithoutAnAuthenticatorIsRefused(t *testing.T) {
	echo := control.FuncHandler(func(_ control.Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	sock, done := udsServerNoAuth(t, echo)
	defer done()

	conn := udsDial(t, sock)
	defer conn.Close()
	writeJSONFrame(t, conn, protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: "rfk_tok"})

	resp := readResponse(t, conn)
	if resp.Success {
		t.Fatal("ctrl_auth was served although no authenticator is configured")
	}
	if resp.Error.Code != protocol.ErrInternal {
		t.Fatalf("code = %q, want %q", resp.Error.Code, protocol.ErrInternal)
	}
	if resp.Error.Message != "identity store unavailable" {
		t.Fatalf("message = %q, want the fixed internal message", resp.Error.Message)
	}
	if _, err := readerFor(conn).ReadFrame(); err == nil {
		t.Fatal("connection stayed open after an uncheckable ctrl_auth")
	}
}

// UDS has no bootstrap window: an authenticator that knows the token admits
// it even while zero users "exist" (CountActive is a TCP-bootstrap concept and
// the UDS handshake must never call it). oneUserNoBootstrap is oneUser with
// the active count at zero.
func TestUDSAuth_IgnoresTheBootstrapCount(t *testing.T) {
	auth := &fakeAuth{
		tokens: map[string]users.Identity{"rfk_good": {UserID: "u1", Username: "brent"}},
		active: 0,
	}
	seen := make(chan control.Connection, 1)
	h := control.FuncHandler(func(c control.Connection, _ []byte) []byte {
		select {
		case seen <- c:
		default:
		}
		return []byte(`{"type":"ctrl_response","command":"ctrl_status","id":"1","success":true}`)
	})
	sock, done := udsServer(t, auth, h)
	defer done()

	conn := udsDial(t, sock)
	defer conn.Close()

	// Auth frame and request in one write, like a real client pipelines.
	var buf bytes.Buffer
	writeFrameTo(t, &buf, protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: "rfk_good"})
	writeFrameTo(t, &buf, map[string]string{"type": protocol.TypeCtrlStatus, "id": "1"})
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatalf("write: %v", err)
	}

	if resp := readResponse(t, conn); !resp.Success {
		t.Fatalf("a token the store knows was refused: %+v", resp.Error)
	}
	c := <-seen
	if c.Identity().UserID != "u1" {
		t.Fatalf("identity = %+v, want u1", c.Identity())
	}
}

// A ctrl_auth frame is consumed by the handshake, so it never reaches the
// dispatcher as a first request — on either path. On the UDS the same holds
// for the anonymous path's PENDING mechanism: the first frame is dispatched
// exactly once, and what follows it is read normally. Both frames in one
// write, so a dropped half shows up as a missing response, not a slow one.
func TestUDSAuth_PipelinedRequestBehindANonAuthFirstFrame(t *testing.T) {
	echo := control.FuncHandler(func(_ control.Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	sock, done := udsServerNoAuth(t, echo)
	defer done()

	conn := udsDial(t, sock)
	defer conn.Close()

	var buf bytes.Buffer
	writeFrameTo(t, &buf, map[string]string{"type": protocol.TypeCtrlStatus, "id": "1"})
	writeFrameTo(t, &buf, map[string]string{"type": protocol.TypeCtrlStatus, "id": "2"})
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// The server echoes the frame bytes verbatim; the expectation is built
	// from the same marshalling so key order can never disagree.
	echoOf := func(id string) string {
		b, err := json.Marshal(map[string]string{"type": protocol.TypeCtrlStatus, "id": id})
		if err != nil {
			t.Fatal(err)
		}
		return "echo:" + string(b)
	}
	if resp := readFrameString(t, conn); resp != echoOf("1") {
		t.Fatalf("first response = %q, want %q", resp, echoOf("1"))
	}
	if resp := readFrameString(t, conn); resp != echoOf("2") {
		t.Fatalf("second response = %q, want %q (the pending frame's follower was dropped)", resp, echoOf("2"))
	}
}

// ─── the cheap invariants ─────────────────────────────────────────────────────

// A ctrl_auth frame AFTER the handshake is an ordinary request to the
// dispatcher, and the dispatcher has no auth verb: it is refused as an
// unknown command type, and the connection's identity is untouched — a
// mid-stream ctrl_auth can never re-authenticate (or re-anonymize) a
// connection whose admission was decided once.
func TestUDSAuth_ALaterCtrlAuthFrameIsInert(t *testing.T) {
	seen := make(chan control.Connection, 4)
	ctrl := &fakeController{}
	sock, done := udsServer(t, oneUser("rfk_good", "brent"), captureDispatchHandler(seen, ctrl))
	defer done()

	conn := udsDial(t, sock)
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	// First: the handshake auth, then an ordinary request.
	var buf bytes.Buffer
	writeFrameTo(t, &buf, protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: "rfk_good"})
	writeFrameTo(t, &buf, map[string]string{"type": protocol.TypeCtrlStatus, "id": "1"})
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatalf("write: %v", err)
	}
	if resp := readResponse(t, conn); !resp.Success {
		t.Fatalf("first request failed: %+v", resp.Error)
	}
	c := <-seen
	if c.Identity().UserID != "u1" {
		t.Fatalf("identity after the handshake = %+v, want u1", c.Identity())
	}

	// Then: another ctrl_auth frame, mid-stream.
	writeJSONFrame(t, conn, protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "2", Token: "rfk_good"})
	resp := readResponse(t, conn)
	if resp.Success || resp.Error.Code != protocol.ErrInvalidArgs {
		t.Fatalf("a mid-stream ctrl_auth = %+v, want an invalid_args refusal", resp)
	}

	// And the identity is exactly what it was.
	writeJSONFrame(t, conn, map[string]string{"type": protocol.TypeCtrlStatus, "id": "3"})
	if resp := readResponse(t, conn); !resp.Success {
		t.Fatalf("request after the refused ctrl_auth failed: %+v", resp.Error)
	}
	c = <-seen
	if c.Identity().UserID != "u1" || c.Identity().Username != "brent" {
		t.Fatalf("identity after the refused ctrl_auth = %+v, want {u1 brent}", c.Identity())
	}
}

// A client that opens the socket and says NOTHING is already in the broadcast
// registry — the optional-auth peek runs after registration and blocks until
// the client speaks, so it cannot delay delivery. This is the attach/subscriber
// pattern: dial, wait for events.
func TestUDSAuth_SilentClientReceivesBroadcasts(t *testing.T) {
	noop := control.FuncHandler(func(control.Connection, []byte) []byte { return nil })
	dir, err := os.MkdirTemp("", "fundi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv, err := control.ListenWithAuth(filepath.Join(dir, "test.sock"), noop, oneUser("secret", "brent"))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn := udsDial(t, filepath.Join(dir, "test.sock"))
	defer conn.Close()

	// No first frame at all — the handshake peek is parked on the read.
	time.Sleep(150 * time.Millisecond)
	sentinel := []byte(`{"type":"ctrl_daemon_shutdown","reason":"test"}`)
	if n := srv.Broadcast(sentinel); n != 1 {
		t.Fatalf("Broadcast reached %d connections, want 1 (the silent client was never registered)", n)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	frame, err := readerFor(conn).ReadFrame()
	if err != nil {
		t.Fatalf("read broadcast: %v", err)
	}
	if string(frame) != string(sentinel) {
		t.Fatalf("broadcast frame = %q", frame)
	}
}

// EOF before any frame — a dial that changed its mind — closes quietly: no
// response, no wedged slot, and the server keeps serving the next client.
func TestUDSAuth_EOFBeforeAnyFrameClosesQuietly(t *testing.T) {
	echo := control.FuncHandler(func(_ control.Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	sock, done := udsServer(t, oneUser("secret", "brent"), echo)
	defer done()

	gone := udsDial(t, sock)
	gone.Close() // EOF on the server's first read

	// The next client still gets served.
	conn := udsDial(t, sock)
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	writeFrame(t, conn, `{"type":"ping"}`)
	if resp := readFrameString(t, conn); resp != `echo:{"type":"ping"}` {
		t.Fatalf("server stopped serving after a silent client vanished: %q", resp)
	}
}

// captureDispatchHandler is a dispatcher-backed handler that also hands the
// connection to seen on every frame, so a test can watch the identity the
// dispatcher actually ran with.
func captureDispatchHandler(seen chan<- control.Connection, ctrl control.Controller) control.ConnectionLifecycleHandler {
	inner := control.NewDispatch(ctrl)
	return captureHandler{seen: seen, inner: inner}
}

type captureHandler struct {
	seen  chan<- control.Connection
	inner control.ConnectionLifecycleHandler
}

func (h captureHandler) HandleFrame(c control.Connection, frame []byte) []byte {
	select {
	case h.seen <- c:
	default:
	}
	return h.inner.HandleFrame(c, frame)
}

func (h captureHandler) HandleClose(c control.Connection) { h.inner.HandleClose(c) }
