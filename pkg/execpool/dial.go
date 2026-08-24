package execpool

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
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
	upConn, err := upgradeconn.Dial(conn, upgradeconn.Executor, host)
	if err != nil {
		return err
	}

	// From here everything reads through upConn. The hello frame and the HTTP/2
	// preface that follows it arrive in one stream, and the upgrade's buffer is
	// part of that stream rather than something to be discarded.
	hello, credential, err := writeHello(upConn, o)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		return err
	}

	if credential != "" && o.CredentialFile != "" {
		// The directory may not exist yet: the default lives under the user's
		// data dir, deliberately NOT under --root, where the executor's own file
		// tools could read it.
		if err := os.MkdirAll(filepath.Dir(o.CredentialFile), 0o700); err != nil {
			return fmt.Errorf("create credential directory: %w", err)
		}
		if err := os.WriteFile(o.CredentialFile, []byte(credential+"\n"), 0600); err != nil {
			return fmt.Errorf("write credential: %w", err)
		}
		slog.Info("executor: enrolled", "id", hello.ExecutorID, "credentialFile", o.CredentialFile)
	}

	return ServeInverted(upConn, o.Handler)
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

// credFileHas reports whether a readable, non-empty credential file exists.
func credFileHas(path string) bool {
	if path == "" {
		return false
	}
	cred, err := readCredential(path)
	return err == nil && cred != ""
}

// buildHello chooses the credential this executor presents.
//
// The ticket case is FIRST and returns immediately: a ticket is mutually
// exclusive with the durable paths, and an interactive client on a machine that
// also runs `rafiki executor serve` will have that executor's credential file
// on disk. Falling through to it would silently connect the session executor as
// the durable one — two identities, one row, and whichever reconnects last wins.
func buildHello(o ConnectOptions) (protocol.ExecutorHelloRequest, error) {
	req := protocol.ExecutorHelloRequest{
		Type:         "executor_hello",
		SelfReported: o.SelfReported,
	}
	switch {
	case o.Ticket != "":
		req.Ticket = o.Ticket
	case o.Credential != "":
		// Supplied directly; no file is read and none will be written.
		req.Credential = o.Credential
	case credFileHas(o.CredentialFile):
		cred, _ := readCredential(o.CredentialFile)
		req.Credential = cred
	case o.EnrollToken != "":
		req.Token = o.EnrollToken
	default:
		return protocol.ExecutorHelloRequest{}, fmt.Errorf(
			"nothing to authenticate with: no session ticket, no --credential, "+
				"no credential file at %s, and no --enroll-token",
			o.CredentialFile)
	}
	return req, nil
}

func writeHello(conn net.Conn, o ConnectOptions) (protocol.ExecutorHelloResponse, string, error) {
	req, err := buildHello(o)
	if err != nil {
		return protocol.ExecutorHelloResponse{}, "", err
	}

	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return protocol.ExecutorHelloResponse{}, "", fmt.Errorf("write hello: %w", err)
	}

	dec := json.NewDecoder(bufio.NewReaderSize(conn, 4096))
	var resp protocol.ExecutorHelloResponse
	if err := dec.Decode(&resp); err != nil {
		return protocol.ExecutorHelloResponse{}, "", fmt.Errorf("read hello response: %w", err)
	}
	if resp.Error != "" {
		// Retryable means rafikid could not CHECK the credential, not that it
		// rejected one. Only the latter is worth exiting over: a revoked row
		// will not un-revoke itself, but a database that is restarting will
		// come back, and an executor that quits over it has turned a blip
		// into an outage — across a fleet reconnecting together, the whole
		// fleet's.
		if resp.Retryable {
			return protocol.ExecutorHelloResponse{}, "", fmt.Errorf("rafikid could not verify the credential: %s", resp.Error)
		}
		return protocol.ExecutorHelloResponse{}, "", fmt.Errorf("%w: %s", ErrEnrollmentRejected, resp.Error)
	}

	return resp, resp.Credential, nil
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
