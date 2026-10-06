package execpool

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
	"net/url"
	"os"
	"path/filepath"
	"time"

	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// ConnectOptions carries everything the executor side needs to dial, enroll,
// and serve on a reverse-dialled connection.
type ConnectOptions struct {
	Addr        string
	ServerName  string
	PinCert     string
	EnrollToken string

	// Credential, when set, is used directly and NOTHING is written to disk.
	// It is how a stateless deployment authenticates: inject the credential
	// from a secret store as an environment variable and mount no volume.
	//
	// Takes precedence over CredentialFile. Enrollment cannot happen on this
	// path — a credential already names a row — so a lost credential is
	// re-issued by the operator rather than re-enrolled by the machine.
	Credential string

	// Ticket authenticates a transient, row-less executor. Mutually exclusive
	// with EnrollToken and Credential; nothing is written to disk on this path
	// because there is nothing durable to keep.
	Ticket string

	// CredentialFile is where an ENROLLED executor persists the credential it
	// was issued, and where it reads it back on reconnect. Empty disables both.
	CredentialFile string
	SelfReported   map[string]string
	Handler        http.Handler

	// SocketPath, when set, dials a unix socket instead of TLS over TCP, and
	// Addr, ServerName and PinCert are ignored.
	//
	// It is for an executor on the daemon's own machine. The point is not to
	// avoid TLS but to avoid the CERTIFICATE: a single-machine install should
	// not need one, and requiring it would make the simplest deployment pay for
	// the most complex. Everything downstream of the upgrade — enrollment, the
	// credential, labels, admission — is identical, so a local executor is a
	// fully rowed executor and not a special case.
	SocketPath string
}

// ErrEnrollmentRejected stops the reconnect loop for good. It means rafikid
// gave an ANSWER about this credential — unknown, consumed, expired, disabled
// — and no amount of retrying changes any of those. Everything else, including
// a store that could not be reached, stays in the loop.
var ErrEnrollmentRejected = errors.New("execpool: enrollment rejected by rafikid")

// Connect dials rafikid, enrolls if needed, and serves the executor's Connect
// handler on the connection until ctx is done.
// Reconnect pacing. Vars rather than constants so tests can exercise the retry
// loop in milliseconds; nothing outside tests changes them.
var (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
)

func Connect(ctx context.Context, o ConnectOptions) error {
	backoff := initialBackoff

	for ctx.Err() == nil {
		err := connectOnce(ctx, o)
		if errors.Is(err, ErrEnrollmentRejected) {
			return err
		}
		slog.Warn("executor: connection lost; reconnecting", "error", err, "in", backoff)
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

func connectOnce(ctx context.Context, o ConnectOptions) error {
	// Built BEFORE dialling: a bad config (nothing to authenticate with)
	// must fail without a network round trip.
	hdr, err := buildAuth(o)
	if err != nil {
		return err
	}

	conn, host, err := dialDaemon(ctx, o)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Reach the executor endpoint by PATH on the shared listener, upgrading out
	// of HTTP/1.1. This is what lets the control plane and the executor link
	// share one port and one certificate; a wrong endpoint now fails with a
	// readable HTTP status instead of as garbage in the first frame.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	upConn, resp, err := upgradeconn.Dial(conn, upgradeconn.Executor, host, hdr)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		return classifyRefusal(err)
	}

	// From here everything reads through upConn — the upgrade's buffer holds
	// whatever the peer pipelined behind the 101, and discarding it would
	// start the HTTP/2 preface mid-frame.
	credential := resp.Get(upgradeconn.HeaderCredential)
	if credential != "" && o.CredentialFile != "" {
		// The directory may not exist yet: the default lives under the user's
		// data dir, deliberately NOT under --root, where the executor's own file
		// tools could read it.
		if err := os.MkdirAll(filepath.Dir(o.CredentialFile), 0o700); err != nil {
			return fmt.Errorf("create credential directory: %w", err)
		}
		if err := os.WriteFile(o.CredentialFile, []byte(credential+"\n"), 0o600); err != nil {
			return fmt.Errorf("write credential: %w", err)
		}
		slog.Info("executor: enrolled", "id", resp.Get(upgradeconn.HeaderExecutorID), "credentialFile", o.CredentialFile)
	}

	return ServeInverted(ctx, upConn, o.Handler)
}

// classifyRefusal turns an upgrade refusal into the one terminal error the
// reconnect loop honours. A 400, 401 or 403 is an ANSWER about this
// credential — unknown, consumed, expired, disabled — and no amount of
// retrying changes any of those. Any other status (a 503 store outage, a 409
// incumbent still answering) returns the error unchanged, which Connect
// treats as retryable.
func classifyRefusal(err error) error {
	var ref *upgradeconn.Refused
	if errors.As(err, &ref) {
		switch ref.Status {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: %s", ErrEnrollmentRejected, ref.Reason)
		}
	}
	return err
}

// dialDaemon opens the transport under the executor link and returns it with
// the Host header value the upgrade request should carry.
//
// Two implementations, one protocol: everything above this function is
// byte-identical on both, which is what keeps a local executor a fully rowed
// executor rather than a second kind of thing.
func dialDaemon(ctx context.Context, o ConnectOptions) (net.Conn, string, error) {
	if o.SocketPath != "" {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", o.SocketPath)
		if err != nil {
			return nil, "", fmt.Errorf("dial %s: %w", o.SocketPath, err)
		}
		// A unix socket has no hostname, but HTTP/1.1 requires a Host header
		// and the daemon's mux routes on path alone. "localhost" is the
		// conventional filler and is never resolved by anything.
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

	tlsCfg := &tls.Config{
		ServerName: sni,
		// http/1.1, not h2: the outer connection is an ordinary HTTP/1.1
		// request that gets UPGRADED, and net/http can only hijack an HTTP/1.1
		// connection. The inverted HTTP/2 begins after the 101, directly on the
		// byte stream, where ALPN plays no part.
		NextProtos: ALPNProtocols,
	}
	if o.PinCert != "" {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // pinned fingerprint
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

// DialDaemon opens the raw connection Connect would use, for callers (the
// sandbox relay) that splice it rather than speak on it.
func DialDaemon(ctx context.Context, o ConnectOptions) (net.Conn, error) {
	c, _, err := dialDaemon(ctx, o)
	return c, err
}

// credFileHas reports whether a readable, non-empty credential file exists.
func credFileHas(path string) bool {
	if path == "" {
		return false
	}
	cred, err := readCredential(path)
	return err == nil && cred != ""
}

// buildAuth builds the headers the upgrade request carries, choosing the
// credential this executor presents.
//
// The ticket case is FIRST and returns immediately: a ticket is mutually
// exclusive with the durable paths, and an interactive client on a machine that
// also runs `rafiki executor serve` will have that executor's credential file
// on disk. Falling through to it would silently connect the session executor as
// the durable one — two identities, one row, and whichever reconnects last wins.
func buildAuth(o ConnectOptions) (http.Header, error) {
	var auth string
	switch {
	case o.Ticket != "":
		auth = string(upgradeconn.SchemeTicket) + " " + o.Ticket
	case o.Credential != "":
		// Supplied directly; no file is read and none will be written.
		auth = string(upgradeconn.SchemeBearer) + " " + o.Credential
	case credFileHas(o.CredentialFile):
		cred, err := readCredential(o.CredentialFile)
		if err != nil {
			return nil, fmt.Errorf("read credential file: %w", err)
		}
		auth = string(upgradeconn.SchemeBearer) + " " + cred
	case o.EnrollToken != "":
		auth = string(upgradeconn.SchemeEnroll) + " " + o.EnrollToken
	default:
		return nil, fmt.Errorf(
			"nothing to authenticate with: no session ticket, no --credential, "+
				"no credential file at %s, and no --enroll-token",
			o.CredentialFile)
	}

	hdr := http.Header{"Authorization": {auth}}
	if len(o.SelfReported) > 0 {
		vals := url.Values{}
		for k, v := range o.SelfReported {
			vals.Set(k, v)
		}
		hdr.Set(upgradeconn.HeaderSelfReported, vals.Encode())
	}
	return hdr, nil
}

func readCredential(path string) (string, error) {
	if path == "" {
		return "", os.ErrNotExist
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s, nil
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
