package control_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// newSelfSignedTLSConfig generates a self-signed ECDSA cert valid for
// localhost and returns a server-side tls.Config.
func newSelfSignedTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"test"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pemEncode("CERTIFICATE", certDER)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pemEncode("EC PRIVATE KEY", keyDER)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

func pemEncode(typ string, der []byte) []byte {
	b := &pem.Block{Type: typ, Bytes: der}
	return pem.EncodeToMemory(b)
}

// fakeAuth is a users.Store-shaped authenticator with no database.
type fakeAuth struct {
	tokens map[string]users.Identity
	active int
	err    error // non-nil = "I could not check", not "invalid"
}

func (f *fakeAuth) Authenticate(_ context.Context, token string) (users.Identity, error) {
	if f.err != nil {
		return users.Identity{}, f.err
	}
	id, ok := f.tokens[token]
	if !ok {
		return users.Identity{}, users.ErrNotFound
	}
	return id, nil
}

func (f *fakeAuth) CountActive(_ context.Context) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.active, nil
}

// oneUser is the ordinary state: a single user whose token authenticates, so
// bootstrap mode is closed.
func oneUser(token, username string) *fakeAuth {
	return &fakeAuth{
		tokens: map[string]users.Identity{token: {UserID: "u1", Username: username}},
		active: 1,
	}
}

// tlsServerHandler starts a TCP+TLS control server with the given
// authenticator and handler. Returns the address (host:port) and a cleanup
// func that calls srv.Close.
func tlsServerHandler(t *testing.T, auth control.Authenticator, h control.ConnectionLifecycleHandler) (addr string, cleanup func()) {
	t.Helper()
	tlsCfg := newSelfSignedTLSConfig(t)
	srv, err := control.ListenTCP("127.0.0.1:0", auth, tlsCfg, h)
	if err != nil {
		t.Fatal(err)
	}
	return srv.Addr().String(), func() { srv.Close() }
}

// tlsServer starts a TCP+TLS server with an echo handler.
func tlsServer(t *testing.T, auth control.Authenticator) (addr string, cleanup func()) {
	t.Helper()
	echo := control.FuncHandler(func(_ control.Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	return tlsServerHandler(t, auth, echo)
}

// tlsDispatchServer starts a TCP+TLS server running the REAL dispatcher, so
// the bootstrap restriction (which lives in dispatch, not in the handshake)
// is exercised end to end.
func tlsDispatchServer(t *testing.T, auth control.Authenticator) (addr string, cleanup func()) {
	t.Helper()
	return tlsServerHandler(t, auth, control.NewDispatch(&fakeController{}))
}

// tlsDial dials a TLS server using an insecure (self-signed) config,
// sends the auth frame, and returns the authed TLS conn. Fatals on error.
func tlsDialAuth(t *testing.T, addr, token string) net.Conn {
	t.Helper()
	cfg := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	writeFrame(t, conn, `{"type":"ctrl_auth","id":"0","token":"`+token+`"}`)
	return conn
}

// tlsDialRaw dials TLS and returns the conn WITHOUT sending auth.
func tlsDialRaw(t *testing.T, addr string) net.Conn {
	t.Helper()
	cfg := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	return conn
}

func writeFrame(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if _, err := conn.Write([]byte(payload + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// writeFrameTo marshals v and appends it as one JSONL frame to w. Separate
// from writeFrame so a test can build several frames into one buffer and send
// them in a SINGLE Write — the pipelining case that a second FrameReader
// silently eats.
func writeFrameTo(t *testing.T, w io.Writer, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// writeJSONFrame writes one marshalled frame to conn.
func writeJSONFrame(t *testing.T, conn net.Conn, v any) {
	t.Helper()
	writeFrameTo(t, conn, v)
}

func readFrameString(t *testing.T, conn net.Conn) string {
	t.Helper()
	fr := protocol.NewFrameReader(conn, protocol.MaxFrameBytes)
	frame, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(frame)
}

// readResponse reads one frame and decodes it as a ctrl_response. Error is
// normalized to a non-nil body so a test can read resp.Error.Code without a
// nil check turning a wrong-code failure into a panic.
func readResponse(t *testing.T, conn net.Conn) protocol.Response {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var resp protocol.Response
	if err := json.Unmarshal([]byte(readFrameString(t, conn)), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error == nil {
		resp.Error = &protocol.ErrorBody{}
	}
	return resp
}

// ─── Tests ─────────────────────────────────────────────────────────────────────

func TestListenTCP_ValidAuthAndEcho(t *testing.T) {
	addr, cleanup := tlsServer(t, oneUser("secret", "brent"))
	defer cleanup()

	conn := tlsDialAuth(t, addr, "secret")
	defer conn.Close()

	writeFrame(t, conn, `{"type":"ctrl_status","id":"1"}`)
	resp := readFrameString(t, conn)
	want := `echo:{"type":"ctrl_status","id":"1"}`
	if resp != want {
		t.Errorf("got %q, want %q", resp, want)
	}
}

func TestListenTCP_WrongToken(t *testing.T) {
	addr, cleanup := tlsServer(t, oneUser("secret", "brent"))
	defer cleanup()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()

	writeFrame(t, conn, `{"type":"ctrl_auth","id":"0","token":"wrong"}`)
	resp := readFrameString(t, conn)
	if !strings.Contains(resp, `"auth_invalid"`) {
		t.Errorf("expected auth_invalid, got: %s", resp)
	}
}

func TestListenTCP_NonAuthFirstFrame(t *testing.T) {
	addr, cleanup := tlsServer(t, oneUser("secret", "brent"))
	defer cleanup()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()

	writeFrame(t, conn, `{"type":"ctrl_status","id":"1"}`)
	resp := readFrameString(t, conn)
	if !strings.Contains(resp, `"auth_required"`) {
		t.Errorf("expected auth_required, got: %s", resp)
	}
}

// TestListenTCP_PipelinedRequestAfterAuth guards against a regression where
// authHandshake's FrameReader (and whatever it had already buffered off the
// socket via its internal bufio.Reader) was discarded when handleConn wrapped
// the raw conn in a brand-new FrameReader. A real client (control.DialURL)
// writes the ctrl_auth frame and returns immediately, so its first real
// request frequently lands in the same TCP segment / same read syscall as
// the auth frame — anything the auth handshake's bufio.Reader pulled off the
// socket past the auth frame's trailing newline must survive into the
// request-handling loop, or the request is silently dropped and the client
// hangs waiting for a response that will never come.
//
// This test writes both frames in a single conn.Write call (no dial/write
// interleaving that a scheduler could paper over) and asserts the server
// still answers the second frame.
func TestListenTCP_PipelinedRequestAfterAuth(t *testing.T) {
	addr, cleanup := tlsServer(t, oneUser("secret", "brent"))
	defer cleanup()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()

	auth := `{"type":"ctrl_auth","id":"0","token":"secret"}` + "\n"
	req := `{"type":"ctrl_status","id":"1"}` + "\n"
	if _, err := conn.Write([]byte(auth + req)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp := readFrameString(t, conn)
	want := `echo:{"type":"ctrl_status","id":"1"}`
	if resp != want {
		t.Fatalf("pipelined request after auth: got %q, want %q (request frame was dropped)", resp, want)
	}
}

func TestListenTCP_PlaintextRefused(t *testing.T) {
	addr, cleanup := tlsServer(t, oneUser("secret", "brent"))
	defer cleanup()

	// Plain TCP dial to TLS port — should get TLS handshake error, not auth error.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("plain dial: %v", err)
	}
	defer conn.Close()

	// Try to write a frame — the server expects TLS, so it'll either reject
	// the ClientHello or timeout. Both are fine; we just want to verify we
	// don't get an auth error (the TLS gate fires first).
	_, writeErr := conn.Write([]byte(`{"type":"ctrl_auth","id":"0","token":"secret"}` + "\n"))
	// Either the write goes on the wire (server sends TLS alert and closes)
	// or the server doesn't respond at all. Either way, the next read should
	// fail, not return an auth frame.
	fr := protocol.NewFrameReader(conn, protocol.MaxFrameBytes)
	_, readErr := fr.ReadFrame()
	if readErr == nil {
		t.Error("expected read error after plaintext to TLS server, got nil")
	}
	_ = writeErr // may succeed (buffered write) or fail
}

// ─── users-backed auth ─────────────────────────────────────────────────────────

func TestValidUserTokenAuthenticatesAndCarriesIdentity(t *testing.T) {
	auth := oneUser("rfk_good", "brent")

	// The channel is what gives the test a happens-before edge on the
	// handler goroutine's read of conn.Identity(); a plain captured
	// variable would be a data race, socket round-trip notwithstanding.
	seen := make(chan control.Connection, 1)
	h := control.FuncHandler(func(c control.Connection, _ []byte) []byte {
		select {
		case seen <- c:
		default:
		}
		return []byte(`{"type":"ctrl_response","command":"ctrl_status","id":"1","success":true}`)
	})
	addr, done := tlsServerHandler(t, auth, h)
	defer done()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()

	// Auth frame and first request in ONE write: a client routinely
	// pipelines them into a single segment, and a second FrameReader built
	// for the request loop would silently discard the request.
	var buf bytes.Buffer
	writeFrameTo(t, &buf, protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: "rfk_good"})
	writeFrameTo(t, &buf, map[string]string{"type": protocol.TypeCtrlStatus, "id": "1"})
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp := readResponse(t, conn)
	if !resp.Success {
		t.Fatalf("status after auth failed: %+v", resp.Error)
	}

	c := <-seen
	if got := c.Identity(); got.UserID != "u1" || got.Username != "brent" {
		t.Fatalf("identity = %+v, want {u1 brent}", got)
	}
	if c.Restricted() {
		t.Fatal("an authenticated connection is bootstrap-restricted")
	}
}

func TestUnknownTokenIsRejectedAsInvalid(t *testing.T) {
	auth := &fakeAuth{tokens: map[string]users.Identity{}, active: 1}
	addr, done := tlsDispatchServer(t, auth)
	defer done()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()
	writeJSONFrame(t, conn, protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: "rfk_bad"})

	resp := readResponse(t, conn)
	if resp.Success || resp.Error.Code != protocol.ErrAuthInvalid {
		t.Fatalf("resp = %+v, want ErrAuthInvalid", resp)
	}
}

// A database blip must not read as a bad credential: a client told 401
// discards a working token and re-prompts.
func TestAStoreOutageIsInternalNotAuthInvalid(t *testing.T) {
	auth := &fakeAuth{err: errors.New("connection refused"), active: 1}
	addr, done := tlsDispatchServer(t, auth)
	defer done()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()
	writeJSONFrame(t, conn, protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: "rfk_good"})

	resp := readResponse(t, conn)
	if resp.Success {
		t.Fatal("auth succeeded during a store outage")
	}
	if resp.Error.Code == protocol.ErrAuthInvalid {
		t.Fatal("a store outage was reported as an invalid token")
	}
	if resp.Error.Code != protocol.ErrInternal {
		t.Fatalf("code = %q, want %q", resp.Error.Code, protocol.ErrInternal)
	}
	// And it must never leak the store's error text: the peer has not proved
	// who it is, and a pgx error carries the DSN.
	if strings.Contains(resp.Error.Message, "connection refused") {
		t.Fatalf("store error text leaked to an unauthenticated peer: %q", resp.Error.Message)
	}
}

// A bootstrap-state lookup that fails is the same "I could not check": it must
// not fall through to admitting the connection.
func TestABootstrapLookupOutageDoesNotAdmit(t *testing.T) {
	auth := &fakeAuth{err: errors.New("connection refused")}
	addr, done := tlsDispatchServer(t, auth)
	defer done()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()
	writeJSONFrame(t, conn, protocol.UserCreateRequest{
		Type: protocol.TypeCtrlUserCreate, ID: "1", Username: "brent",
	})

	resp := readResponse(t, conn)
	if resp.Success {
		t.Fatal("user_create was served while the identity store was unreachable")
	}
	if resp.Error.Code != protocol.ErrInternal {
		t.Fatalf("code = %q, want %q", resp.Error.Code, protocol.ErrInternal)
	}
	if strings.Contains(resp.Error.Message, "connection refused") {
		t.Fatalf("store error text leaked to an unauthenticated peer: %q", resp.Error.Message)
	}
}

func TestBootstrapAdmitsUserCreateWithoutAToken(t *testing.T) {
	auth := &fakeAuth{tokens: map[string]users.Identity{}, active: 0}
	addr, done := tlsDispatchServer(t, auth)
	defer done()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()
	// No ctrl_auth frame at all — straight to the one permitted command.
	writeJSONFrame(t, conn, protocol.UserCreateRequest{
		Type: protocol.TypeCtrlUserCreate, ID: "1", Username: "brent",
	})

	resp := readResponse(t, conn)
	if !resp.Success {
		t.Fatalf("bootstrap user_create failed: %+v", resp.Error)
	}
}

// The bootstrap frame is read by the HANDSHAKE and cannot be read off the
// socket again, so it is handed to the request loop as a pending frame. Any
// bytes the client pipelined behind it live in that same FrameReader. This
// sends both in ONE write: if either half is dropped, one of the two
// responses never arrives and the test times out on the read deadline.
func TestBootstrapDispatchesThePendingFrameAndWhatFollowsIt(t *testing.T) {
	auth := &fakeAuth{tokens: map[string]users.Identity{}, active: 0}
	addr, done := tlsDispatchServer(t, auth)
	defer done()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()

	var buf bytes.Buffer
	writeFrameTo(t, &buf, protocol.UserCreateRequest{
		Type: protocol.TypeCtrlUserCreate, ID: "1", Username: "brent",
	})
	writeFrameTo(t, &buf, map[string]string{"type": protocol.TypeCtrlStatus, "id": "2"})
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatalf("write: %v", err)
	}

	first := readResponse(t, conn)
	if first.ID != "1" || !first.Success {
		t.Fatalf("first response = %+v, want id=1 success", first)
	}
	// The second frame is refused by the restriction, but a REFUSAL proves
	// it was read; a dropped frame produces no response at all.
	second := readResponse(t, conn)
	if second.ID != "2" {
		t.Fatalf("second response = %+v, want id=2", second)
	}
	if second.Error.Code != protocol.ErrAuthRequired {
		t.Fatalf("second code = %q, want %q", second.Error.Code, protocol.ErrAuthRequired)
	}
}

func TestBootstrapRejectsEveryOtherCommand(t *testing.T) {
	auth := &fakeAuth{tokens: map[string]users.Identity{}, active: 0}
	addr, done := tlsDispatchServer(t, auth)
	defer done()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()
	writeJSONFrame(t, conn, map[string]string{"type": protocol.TypeCtrlStatus, "id": "1"})

	resp := readResponse(t, conn)
	if resp.Success {
		t.Fatal("ctrl_status was accepted on an unauthenticated bootstrap connection")
	}
	if resp.Error.Code != protocol.ErrAuthRequired {
		t.Fatalf("code = %q, want %q", resp.Error.Code, protocol.ErrAuthRequired)
	}
}

// Bootstrap is a property of "no users", not of the connection: once a user
// exists the same connection shape must present a token.
func TestOnceAUserExistsBootstrapIsClosed(t *testing.T) {
	auth := &fakeAuth{tokens: map[string]users.Identity{}, active: 1}
	addr, done := tlsDispatchServer(t, auth)
	defer done()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()
	writeJSONFrame(t, conn, protocol.UserCreateRequest{
		Type: protocol.TypeCtrlUserCreate, ID: "1", Username: "mallory",
	})

	resp := readResponse(t, conn)
	if resp.Success {
		t.Fatal("unauthenticated user_create succeeded while a user exists")
	}
	if resp.Error.Code != protocol.ErrAuthRequired {
		t.Fatalf("code = %q, want %q", resp.Error.Code, protocol.ErrAuthRequired)
	}
}
