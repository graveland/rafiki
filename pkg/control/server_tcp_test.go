package control_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
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

// tlsServer starts a TCP+TLS server with the given token and an echo handler.
// Returns the address (host:port) and a cleanup func that calls srv.Close.
func tlsServer(t *testing.T, token string) (addr string, cleanup func()) {
	t.Helper()
	tlsCfg := newSelfSignedTLSConfig(t)
	echo := control.FuncHandler(func(_ control.Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	srv, err := control.ListenTCP("127.0.0.1:0", token, tlsCfg, echo)
	if err != nil {
		t.Fatal(err)
	}
	return srv.Addr().String(), func() { srv.Close() }
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

func readFrameString(t *testing.T, conn net.Conn) string {
	t.Helper()
	fr := protocol.NewFrameReader(conn, protocol.MaxFrameBytes)
	frame, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(frame)
}

// ─── Tests ─────────────────────────────────────────────────────────────────────

func TestListenTCP_ValidAuthAndEcho(t *testing.T) {
	addr, cleanup := tlsServer(t, "secret")
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
	addr, cleanup := tlsServer(t, "secret")
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
	addr, cleanup := tlsServer(t, "secret")
	defer cleanup()

	conn := tlsDialRaw(t, addr)
	defer conn.Close()

	writeFrame(t, conn, `{"type":"ctrl_status","id":"1"}`)
	resp := readFrameString(t, conn)
	if !strings.Contains(resp, `"auth_required"`) {
		t.Errorf("expected auth_required, got: %s", resp)
	}
}

func TestListenTCP_PlaintextRefused(t *testing.T) {
	addr, cleanup := tlsServer(t, "secret")
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