// Package daraja hosts a single child process (Claude Code) and relays its
// stdio over a reverse-dialled connection to rafikid — the same inversion the
// executor uses, so a laptop behind NAT can reach an operator's daemon.
package daraja

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.graveland.dev/rafiki/pkg/upgradeconn"
	"golang.org/x/net/http2"
)

// ConnectOptions carries everything daraja needs to dial rafikid and serve
// DarajaService on the resulting connection.
type ConnectOptions struct {
	Addr, SocketPath, ServerName, PinCert string
	ChildID, Ticket                       string
	Handler                               http.Handler

	// Credential is the reconnect credential the daemon minted on the first
	// successful upgrade (riding the 101 as Rafiki-Credential). It is kept in
	// memory only — claude dies with daraja, so persisting it would name
	// something that no longer exists.
	Credential string
}

// ErrRejected stops the reconnect loop for good. It means rafikid gave an
// ANSWER about this credential — unknown, consumed, expired — and no amount
// of retrying changes any of those. Everything else, including a store that
// could not be reached, stays in the loop.
var ErrRejected = errors.New("daraja: rejected by rafikid")

// backoff pacing. Vars rather than constants so tests can exercise the retry
// loop in milliseconds.
var (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
)

// Connect dials rafikid and serves the handler on the connection until ctx is
// done or a terminal auth error arrives. Reconnect loop modelled on execpool.
func Connect(ctx context.Context, o ConnectOptions) error {
	backoff := initialBackoff

	for ctx.Err() == nil {
		cred, err := connectOnce(ctx, o)
		if cred != "" {
			o.Credential = cred
			o.Ticket = "" // mutual exclusion: credential replaces ticket
		}
		if errors.Is(err, ErrRejected) {
			return err
		}
		slog.Warn("daraja: connection lost; reconnecting", "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return ctx.Err()
}

func connectOnce(ctx context.Context, o ConnectOptions) (cred string, err error) {
	// Authenticate on the upgrade request itself, before the 101: exactly one
	// Authorization header — the ticket on the first dial, the credential the
	// daemon minted on every later one — plus the child id the credential is
	// checked against. Built before dialling, so a daraja with nothing to
	// present never opens a connection at all.
	hdr := http.Header{upgradeconn.HeaderChildID: {o.ChildID}}
	switch {
	case o.Ticket != "":
		hdr.Set("Authorization", string(upgradeconn.SchemeTicket)+" "+o.Ticket)
	case o.Credential != "":
		hdr.Set("Authorization", string(upgradeconn.SchemeBearer)+" "+o.Credential)
	default:
		return "", fmt.Errorf("nothing to authenticate with: no ticket and no credential")
	}

	conn, host, err := dialDaemon(ctx, o)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// Reach the /daraja/connect endpoint by PATH on the shared listener,
	// upgrading out of HTTP/1.1. Same shape as executor's connectOnce.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	upConn, resp, err := upgradeconn.Dial(conn, upgradeconn.Daraja, host, hdr)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		var ref *upgradeconn.Refused
		if errors.As(err, &ref) && (ref.Status == 400 || ref.Status == 401 || ref.Status == 403) {
			// A definitive answer about this credential: bad, revoked, spent
			// or unknown. Retrying cannot change it — end the loop.
			return "", fmt.Errorf("%w: %s", ErrRejected, ref.Reason)
		}
		return "", err // anything else (including 503) is retried by Connect
	}

	if c := resp.Get(upgradeconn.HeaderCredential); c != "" {
		cred = c
	}

	err = ServeInverted(upConn, o.Handler)
	return cred, err
}

// dialDaemon opens a transport connection (unix or TLS) to rafikid.
// Two paths, one protocol — mirroring execpool.dialDaemon.
func dialDaemon(ctx context.Context, o ConnectOptions) (net.Conn, string, error) {
	if o.SocketPath != "" {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", o.SocketPath)
		if err != nil {
			return nil, "", fmt.Errorf("dial %s: %w", o.SocketPath, err)
		}
		return conn, "localhost", nil
	}

	host, _, err := net.SplitHostPort(o.Addr)
	if err != nil {
		host = o.Addr
	}
	sni := o.ServerName
	if sni == "" {
		sni = host
	}

	// System-roots verification by default, mirroring execpool's dial posture:
	// a peer presenting a certificate no trusted root vouches for is REFUSED,
	// not waved through. Pinning REPLACES that verification with the pinned
	// leaf-fingerprint check — the deliberate posture of an executor started
	// with --pin-cert for a self-signed or internal-CA daemon. The old
	// placeholder set InsecureSkipVerify unconditionally, which made the
	// unpinned posture verify nothing at all — acceptable when this dial
	// carried only the one-shot ticket, and no longer acceptable now that a
	// script child's per-child credential rides the same listener.
	tlsCfg := &tls.Config{
		ServerName: sni,
		// http/1.1, not h2: the outer connection is an ordinary HTTP/1.1
		// request that gets UPGRADED (the inverted h2 begins after the 101),
		// so ALPN must not negotiate h2 here.
		NextProtos: []string{"http/1.1"},
	}
	if o.PinCert != "" {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // replaced by the pinned fingerprint below
		tlsCfg.VerifyPeerCertificate = pinVerify(o.PinCert)
	}

	conn, err := tls.DialWithDialer(&net.Dialer{}, "tcp", o.Addr, tlsCfg)
	if err != nil {
		return nil, "", fmt.Errorf("dial %s: %w", o.Addr, err)
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, "", fmt.Errorf("tls handshake: %w", err)
	}
	return conn, sni, nil
}

// ServeInverted runs an HTTP/2 server on a connection this process DIALLED.
// Mirrors execpool.ServeInverted exactly.
func ServeInverted(conn net.Conn, handler http.Handler) error {
	srv := &http2.Server{}
	srv.ServeConn(conn, &http2.ServeConnOpts{
		Handler: handler,
		Context: context.Background(),
	})
	return nil
}

func pinVerify(wantHex string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("pin: no certificate presented")
		}
		h := sha256.Sum256(rawCerts[0])
		got := fmt.Sprintf("%x", h[:])
		if got != wantHex {
			return fmt.Errorf("pin: certificate fingerprint differs")
		}
		return nil
	}
}

// TLSTransport returns an http.RoundTripper speaking TLS to the daemon's
// control listener with the SAME verification posture dialDaemon uses: system
// roots by default, or — for a self-signed or internal-CA daemon — the leaf
// certificate verified by pinned SHA-256 fingerprint when pinCert is given
// (the posture a --pin-cert executor itself dials with). ServerName is the
// SNI to present when it differs from the host in the URL the transport
// dials. An unpinned transport therefore REFUSES a certificate no trusted
// root vouches for — the per-child credential injected on this hop must not
// ride a connection whose peer was never verified.
//
// This is the transport for the executor-hosted per-child socket's proxy
// target: the face lives on the same listener this process reverse-dials, so
// a pinned daemon must be verified by the same pin for both channels or one
// of them has no trust at all. No HTTP/2 is forced: the listener advertises
// http/1.1 over ALPN (its h2 is reserved for the INVERTED upgrade
// connections), and Connect's streaming rides HTTP/1.1 chunking fine.
func TLSTransport(serverName, pinCert string) http.RoundTripper {
	tlsCfg := &tls.Config{
		ServerName: serverName,
	}
	if pinCert != "" {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // replaced by the pinned fingerprint below
		tlsCfg.VerifyPeerCertificate = pinVerify(pinCert)
	}
	return &http.Transport{TLSClientConfig: tlsCfg}
}
