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
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// ConnectOptions carries everything the executor side needs to dial, enroll,
// and serve on a reverse-dialled connection.
type ConnectOptions struct {
	Addr           string
	ServerName     string
	PinCert        string
	EnrollToken    string
	CredentialFile string
	SelfReported   map[string]string
	Handler        http.Handler
}

var ErrEnrollmentRejected = errors.New("execpool: enrollment rejected by rafikid")

// Connect dials rafikid, enrolls if needed, and serves the executor's Connect
// handler on the connection until ctx is done.
func Connect(ctx context.Context, o ConnectOptions) error {
	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

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
		NextProtos: ALPNProtocols,
	}
	if o.PinCert != "" {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // pinned fingerprint
		tlsCfg.VerifyPeerCertificate = pinVerify(o.PinCert)
	}

	conn, err := tls.DialWithDialer(&net.Dialer{}, "tcp", o.Addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("dial %s: %w", o.Addr, err)
	}
	defer conn.Close()

	if err := conn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("tls handshake: %w", err)
	}
	if got := conn.ConnectionState().NegotiatedProtocol; got != "h2" {
		return fmt.Errorf("execpool: ALPN negotiated %q, want h2", got)
	}

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	hello, credential, err := writeHello(conn, o)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		return err
	}

	if credential != "" {
		if err := os.WriteFile(o.CredentialFile, []byte(credential+"\n"), 0600); err != nil {
			return fmt.Errorf("write credential: %w", err)
		}
		slog.Info("executor: enrolled", "id", hello.ExecutorID, "credentialFile", o.CredentialFile)
	}

	return ServeInverted(conn, o.Handler)
}

func writeHello(conn net.Conn, o ConnectOptions) (protocol.ExecutorHelloResponse, string, error) {
	var req protocol.ExecutorHelloRequest
	if cred, err := readCredential(o.CredentialFile); err == nil && cred != "" {
		req.Credential = cred
	} else if o.EnrollToken != "" {
		req.Token = o.EnrollToken
	} else {
		return protocol.ExecutorHelloResponse{}, "", fmt.Errorf("no credential file at %s and no enroll token; cannot authenticate", o.CredentialFile)
	}
	req.Type = "executor_hello"
	req.SelfReported = o.SelfReported

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
