// SPDX-License-Identifier: Apache-2.0

package daraja

// The TLS verification posture of every outbound channel this process opens
// toward the daemon: system roots by default, the pinned fingerprint when one
// is configured — and NEVER "verify nothing". The pre-wave-4 placeholder set
// InsecureSkipVerify unconditionally, which made an unpinned-TLS executor's
// per-child proxy (whose requests carry the child secret's Bearer header)
// accept any certificate while the executor's own credential rode a verified
// connection. These tests pin the corrected posture against a real
// self-signed listener, for both the transport the childsock proxy uses and
// the raw dial the reverse channel uses.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

// selfSignedListener serves TLS with a self-signed certificate no system root
// vouches for, and returns the listener, the leaf's SHA-256 fingerprint (the
// --pin-cert spelling), and a teardown func.
func selfSignedListener(t *testing.T) (net.Listener, string, func()) { //nolint:unparam // teardown mirrors the package's listener helpers
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "rafiki-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// A minimal HTTP/1.1 responder: a completed RoundTrip is the proof the
	// handshake was accepted, not merely attempted.
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})}
	go func() { _ = srv.Serve(ln) }()
	sum := sha256.Sum256(der)
	return ln, fmt.Sprintf("%x", sum[:]), func() { _ = srv.Close() }
}

// An unpinned transport must REFUSE a self-signed peer: system roots are the
// verification, not a formality.
func TestTLSTransportUnpinnedRefusesSelfSigned(t *testing.T) {
	ln, _, teardown := selfSignedListener(t)
	defer teardown()

	_, err := TLSTransport("", "").RoundTrip(probeRequest(t, ln.Addr().String()))
	if err == nil {
		t.Fatal("an unpinned transport accepted a self-signed certificate; system-roots verification is not optional")
	}
}

// A pinned transport accepts the pinned leaf and refuses a DIFFERENT
// self-signed one — the fingerprint is the whole decision.
func TestTLSTransportPinnedVerifiesTheFingerprint(t *testing.T) {
	ln, fp, teardown := selfSignedListener(t)
	defer teardown()
	addr := ln.Addr().String()

	if _, err := TLSTransport("", fp).RoundTrip(probeRequest(t, addr)); err != nil {
		t.Fatalf("a transport pinned to the serving leaf's fingerprint refused it: %v", err)
	}
	if _, err := TLSTransport("", pinnedOtherFingerprint).RoundTrip(probeRequest(t, addr)); err == nil {
		t.Fatal("a transport pinned to another fingerprint accepted the peer")
	}
}

// The reverse channel's dial shares the posture: unpinned refuses, pinned to
// the leaf's fingerprint completes the handshake.
func TestDialDaemonVerifiesLikeTheTransport(t *testing.T) {
	ln, fp, teardown := selfSignedListener(t)
	defer teardown()
	addr := ln.Addr().String()

	if _, _, err := dialDaemon(context.Background(), ConnectOptions{Addr: addr}); err == nil {
		t.Fatal("an unpinned daraja dial accepted a self-signed certificate")
	}
	conn, _, err := dialDaemon(context.Background(), ConnectOptions{Addr: addr, PinCert: fp})
	if err != nil {
		t.Fatalf("a pinned daraja dial refused the leaf it was pinned to: %v", err)
	}
	_ = conn.Close()
}

const pinnedOtherFingerprint = "0000000000000000000000000000000000000000000000000000000000000000"

// probeRequest builds one request for the transport without any real handler
// on the other end — the TLS handshake outcome is the assertion, so the
// server side just closes.
func probeRequest(t *testing.T, addr string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://"+addr+"/probe", nil)
	if err != nil {
		t.Fatalf("build probe request: %v", err)
	}
	return req
}
