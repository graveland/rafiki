package client_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

func newSelfSignedConfig(t *testing.T) *tls.Config {
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
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// oneUserAuth authenticates exactly one token and reports one active user, so
// the listener is out of bootstrap mode.
type oneUserAuth struct{ token string }

func (a oneUserAuth) Authenticate(_ context.Context, token string) (users.Identity, error) {
	if token != a.token {
		return users.Identity{}, users.ErrNotFound
	}
	return users.Identity{UserID: "u1", Username: "brent"}, nil
}

func (oneUserAuth) CountActive(context.Context) (int, error) { return 1, nil }

// zeroUsersAuth reports no active users, which puts ListenTCP's authHandshake
// into bootstrap mode: a first frame that is NOT ctrl_auth is admitted and
// handed back to be dispatched as the request, rather than refused.
type zeroUsersAuth struct{}

func (zeroUsersAuth) Authenticate(_ context.Context, _ string) (users.Identity, error) {
	return users.Identity{}, users.ErrNotFound
}

func (zeroUsersAuth) CountActive(context.Context) (int, error) { return 0, nil }

func startEchoTCPServer(t *testing.T, token string) (addr string, cleanup func()) {
	t.Helper()
	tlsCfg := newSelfSignedConfig(t)
	echo := control.FuncHandler(func(_ control.Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	srv, err := control.ListenTCP("127.0.0.1:0", oneUserAuth{token: token}, tlsCfg, echo)
	if err != nil {
		t.Fatal(err)
	}
	return srv.Addr().String(), func() { srv.Close() }
}

// startBootstrapTCPServer reports zero users, so the daemon is in bootstrap
// mode: it admits a connection whose first frame is not ctrl_auth. The
// handler echoes back whether the connection arrived Restricted(), which is
// how a bootstrap admission is distinguished from an authenticated one.
func startBootstrapTCPServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	tlsCfg := newSelfSignedConfig(t)
	handler := control.FuncHandler(func(conn control.Connection, frame []byte) []byte {
		restricted := "0"
		if conn.Restricted() {
			restricted = "1"
		}
		return append([]byte("restricted="+restricted+":"), frame...)
	})
	srv, err := control.ListenTCP("127.0.0.1:0", zeroUsersAuth{}, tlsCfg, handler)
	if err != nil {
		t.Fatal(err)
	}
	return srv.Addr().String(), func() { srv.Close() }
}

func writeFrame(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if _, err := conn.Write([]byte(payload + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
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

func TestDialURL_BadURL(t *testing.T) {
	ctx := context.Background()
	_, err := client.DialURL(ctx, "http://example.com:1234", "token")
	if err == nil {
		t.Fatal("expected error for http:// scheme")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("expected 'https' in error, got: %v", err)
	}

	_, err = client.DialURL(ctx, "https://", "token")
	if err == nil {
		t.Fatal("expected error for missing host")
	}
}

func TestDialURL_SelfSignedCertRejected(t *testing.T) {
	// DialURL uses system roots. A self-signed cert should fail TLS verification.
	addr, cleanup := startEchoTCPServer(t, "secret")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.DialURL(ctx, "https://"+addr, "secret")
	if err == nil {
		t.Fatal("expected TLS error against self-signed cert")
	}
}

// TestDialURL_ManualAuth tests auth end-to-end through a manual TLS dial
// (bypassing DialURL which wants system roots). Verifies the auth protocol
// end-to-end through the real ListenTCP.
func TestDialURL_ManualAuth(t *testing.T) {
	addr, cleanup := startEchoTCPServer(t, "secret")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialer := &tls.Dialer{Config: &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Send auth frame.
	authReq := protocol.AuthRequest{Type: protocol.TypeCtrlAuth, ID: "0", Token: "secret"}
	authB, _ := json.Marshal(authReq)
	writeFrame(t, conn, string(authB))

	// Auth success is implicit — server doesn't send a response.
	// Send a real command and verify echo.
	writeFrame(t, conn, `{"type":"hello","id":"1"}`)
	echo := readFrameString(t, conn)
	if want := `echo:{"type":"hello","id":"1"}`; echo != want {
		t.Errorf("after auth: got %q, want %q", echo, want)
	}
}

// TestDialURL_WrongToken verifies the server closes the conn on wrong token.
func TestDialURL_WrongToken(t *testing.T) {
	addr, cleanup := startEchoTCPServer(t, "secret")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialer := &tls.Dialer{Config: &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Send wrong token.
	writeFrame(t, conn, `{"type":"ctrl_auth","id":"0","token":"wrong"}`)

	// Server should send an error frame and close.
	fr := protocol.NewFrameReader(conn, protocol.MaxFrameBytes)
	frame, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("expected error frame, got read error: %v", err)
	}
	if !strings.Contains(string(frame), `"auth_invalid"`) {
		t.Errorf("expected auth_invalid, got: %s", string(frame))
	}

	// Subsequent read should fail (server closed).
	_, err = fr.ReadFrame()
	if err == nil {
		t.Fatal("expected read error after auth failure, got nil")
	}
}

// TestNoTokenDial_BootstrapAdmitsTheFirstRequest exercises the no-token wire
// contract DialURL now implements: when token is empty, no ctrl_auth frame is
// written, so the caller's first request lands as the daemon's very first
// frame. On a bootstrap daemon (no users yet) that is exactly what admits the
// connection and makes `rafiki user create` reachable on a fresh remote
// daemon in the first place.
//
// This dials manually (InsecureSkipVerify) rather than through DialURL,
// because DialURL only ever trusts system root CAs and a self-signed test
// certificate can never pass that check — see TestDialURL_SelfSignedCertRejected.
// TestSendAuthFrame_EmptyTokenWritesNothing (url_internal_test.go) is the
// companion test that DialURL's own code took this branch; this test proves
// the resulting wire behavior is what the daemon actually wants.
func TestNoTokenDial_BootstrapAdmitsTheFirstRequest(t *testing.T) {
	addr, cleanup := startBootstrapTCPServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialer := &tls.Dialer{Config: &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// No ctrl_auth frame — this is the no-token DialURL path. The very first
	// frame on the wire is the caller's real request.
	writeFrame(t, conn, `{"type":"ctrl_user_create","id":"1"}`)

	got := readFrameString(t, conn)
	if want := `restricted=1:{"type":"ctrl_user_create","id":"1"}`; got != want {
		t.Errorf("bootstrap dispatch = %q, want %q", got, want)
	}
}

// TestNoTokenDial_NonBootstrapRejectsWithAuthRequired is the other half of
// the no-token contract: once a user exists, a non-ctrl_auth first frame is
// not bootstrap-eligible, and the daemon must refuse it with auth_required —
// never silently paper over the missing credential.
func TestNoTokenDial_NonBootstrapRejectsWithAuthRequired(t *testing.T) {
	addr, cleanup := startEchoTCPServer(t, "secret")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialer := &tls.Dialer{Config: &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// No ctrl_auth frame, and this daemon is NOT in bootstrap (one user
	// exists per oneUserAuth.CountActive).
	writeFrame(t, conn, `{"type":"hello","id":"1"}`)

	fr := protocol.NewFrameReader(conn, protocol.MaxFrameBytes)
	frame, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("expected error frame, got read error: %v", err)
	}
	if !strings.Contains(string(frame), `"auth_required"`) {
		t.Errorf("expected auth_required, got: %s", string(frame))
	}

	_, err = fr.ReadFrame()
	if err == nil {
		t.Fatal("expected read error after auth_required (server closed), got nil")
	}
}
