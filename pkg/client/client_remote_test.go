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

func startEchoTCPServer(t *testing.T, token string) (addr string, cleanup func()) {
	t.Helper()
	tlsCfg := newSelfSignedConfig(t)
	echo := control.FuncHandler(func(_ control.Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	})
	srv, err := control.ListenTCP("127.0.0.1:0", token, tlsCfg, echo)
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
	if !strings.Contains(err.Error(), "tls") {
		t.Errorf("expected 'tls' in error, got: %v", err)
	}

	_, err = client.DialURL(ctx, "tls://", "token")
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

	_, err := client.DialURL(ctx, "tls://"+addr, "secret")
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