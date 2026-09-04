// Package daraja hosts a single child process (Claude Code) and relays its
// stdio over a reverse-dialled connection to rafikid — the same inversion the
// executor uses, so a laptop behind NAT can reach an operator's daemon.
package daraja

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
	"golang.org/x/net/http2"
)

// ConnectOptions carries everything daraja needs to dial rafikid and serve
// DarajaService on the resulting connection.
type ConnectOptions struct {
	Addr, SocketPath, ServerName, PinCert string
	ChildID, Ticket                       string
	Handler                               http.Handler
	PID                                   int

	// Credential is the reconnect credential the daemon returned in the first
	// hello response. It is kept in memory only — claude dies with daraja, so
	// persisting it would name something that no longer exists.
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
	conn, host, err := dialDaemon(ctx, o)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// Reach the /daraja/connect endpoint by PATH on the shared listener,
	// upgrading out of HTTP/1.1. Same shape as executor's connectOnce.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	upConn, err := upgradeconn.Dial(conn, upgradeconn.Daraja, host)
	if err != nil {
		return "", err
	}

	// The reader returned by writeHello must be served, not discarded.
	// Task 1 bug: if we build a fresh bufio.Reader off upConn.Conn we drop
	// whatever pipelined behind the hello response (the h2 preface).
	rd, hello, err := writeHello(upConn, o)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		return "", err
	}

	if hello.Credential != "" {
		cred = hello.Credential
	}

	err = ServeInverted(readerConn{Conn: upConn, r: rd}, o.Handler)
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

	tlsCfg := &tls.Config{
		ServerName:         sni,
		NextProtos:         []string{"http/1.1"}, // outer is http/1.1; inverted h2 begins after 101
		InsecureSkipVerify: true,                 // placeholder — PinCert check below
	}
	if o.PinCert != "" {
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

// writeHello sends the DarajaHelloRequest and reads the response, returning
// the READER it used along with the outcome.
//
// Three differences from execpool/writeHello, each carrying its own comment:
//
//  1. The hello is protocol.DarajaHelloRequest, carrying ChildID and switching
//     from Ticket to Credential after the first success.
//
//  2. The reader returned by this function is what gets served on the
//     connection — do NOT derive a new reader off the bare Conn. The executor
//     link had this bug before Task 1 fixed it; see execpool/dial.go for the
//     rationale.
//
//  3. A terminal error ends the loop AND the process: daraja dies with its
//     child. Reuse the Retryable discrimination — Retryable=false means rafikid
//     answered definitively ("not valid"); Retryable=true means it could not
//     check ("could not verify"), which is transient and worth retrying.
func writeHello(conn io.ReadWriteCloser, o ConnectOptions) (io.Reader, protocol.DarajaHelloResponse, error) {
	req := protocol.DarajaHelloRequest{
		Type:    "daraja_hello",
		ChildID: o.ChildID,
		PID:     o.PID,
	}
	switch {
	case o.Ticket != "":
		req.Ticket = o.Ticket
	case o.Credential != "":
		req.Credential = o.Credential
	default:
		return nil, protocol.DarajaHelloResponse{}, fmt.Errorf(
			"nothing to authenticate with: no ticket and no credential")
	}

	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return nil, protocol.DarajaHelloResponse{}, fmt.Errorf("write hello: %w", err)
	}

	br := bufio.NewReaderSize(conn, 4096)
	dec := json.NewDecoder(br)
	var resp protocol.DarajaHelloResponse
	if err := dec.Decode(&resp); err != nil {
		return nil, protocol.DarajaHelloResponse{}, fmt.Errorf("read hello response: %w", err)
	}
	if resp.Error != "" {
		// Retryable discriminates "I could not CHECK" from "this is NOT valid".
		// Only the latter is worth exiting over: a transient failure should be
		// retried; a definitive rejection (bad token, revoked row) must stop
		// immediately — daraja dies with its child.
		if resp.Retryable {
			return nil, protocol.DarajaHelloResponse{}, fmt.Errorf("rafikid could not verify credential: %s", resp.Error)
		}
		return nil, protocol.DarajaHelloResponse{}, fmt.Errorf("%w: %s", ErrRejected, resp.Error)
	}

	// json.Decoder buffers too. Anything it read past the response's newline is
	// in its Buffered() reader, ahead of whatever is still in br.
	mr := io.MultiReader(dec.Buffered(), br)

	// Consume the '\n' delimiter that terminates the hello response.
	// Since json.Decoder parses the JSON object, it leaves the trailing newline
	// in the buffer. We must consume up to and including that newline so the
	// returned reader starts exactly at the next frame (the HTTP/2 preface).
	var singleByte [1]byte
	for {
		_, err := mr.Read(singleByte[:])
		if err != nil {
			break
		}
		if singleByte[0] == '\n' {
			break
		}
	}

	return mr, resp, nil
}

// readerConn is a net.Conn whose reads come from r — the hello reader, which
// already holds whatever the peer pipelined behind its response. Writes and
// the rest of the Conn behaviour pass through unchanged. Mirrors execpool's
// readerConn exactly.
type readerConn struct {
	net.Conn
	r io.Reader
}

func (c readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

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
