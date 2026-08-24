package execpool

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// A store that cannot be READ is not a store that REJECTED you. Conflating the
// two made every executor reconnecting during a Postgres restart exit
// permanently — and executors reconnect together, so one transient failure
// took the whole fleet out until somebody noticed and restarted each machine
// by hand.
func TestTransientAuthFailureIsRetriedRatherThanDowningTheFleet(t *testing.T) {
	store := newFakeStore("exec-1")
	store.authErr = errors.New("failed to connect to `host=db.internal user=rafiki`: connection refused")

	addr, pin, _ := servePool(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := Connect(ctx, connectOpts(t, addr, pin))

	if errors.Is(err, ErrEnrollmentRejected) {
		t.Fatal("a store that could not be read was treated as a rejected credential; " +
			"the executor gave up permanently over a transient failure")
	}
	if n := len(store.authCalls); n < 2 {
		t.Fatalf("Authenticate was attempted %d time(s); a transient failure must be RETRIED", n)
	}
}

// The other half, which the fix must not break: a genuinely revoked row still
// stops the executor rather than spinning forever.
func TestRevokedCredentialStillStopsTheExecutor(t *testing.T) {
	store := newFakeStore("exec-1")
	store.authErr = executors.ErrDisabled

	addr, pin, _ := servePool(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err := Connect(ctx, connectOpts(t, addr, pin))

	if !errors.Is(err, ErrEnrollmentRejected) {
		t.Fatalf("a revoked executor must stop, not retry: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("a terminal rejection must stop immediately, not after backoff")
	}
	if n := len(store.authCalls); n != 1 {
		t.Errorf("a terminal rejection must not be retried; got %d attempts", n)
	}
}

// The peer on the other end of a failed Authenticate has by definition not
// proved who it is. Forwarding the store's error handed it whatever the driver
// put in the message — here a DSN with a host and a username.
func TestRetryableFailureDoesNotLeakTheStoreError(t *testing.T) {
	store := newFakeStore("exec-1")
	store.authErr = errors.New("failed to connect to `host=db.internal user=rafiki`: connection refused")

	resp := helloExchange(t, store, protocol.ExecutorHelloRequest{Credential: "c"})

	if !resp.Retryable {
		t.Error("a store failure must be marked retryable")
	}
	for _, leak := range []string{"db.internal", "user=rafiki", "connection refused"} {
		if strings.Contains(resp.Error, leak) {
			t.Errorf("the response leaked %q to an unauthenticated peer: %s", leak, resp.Error)
		}
	}
}

// A terminal rejection may say what it is: the text comes from our own
// sentinels, and an operator staring at a machine that will not join needs to
// know it was disabled rather than unreachable.
func TestTerminalRejectionNamesTheReason(t *testing.T) {
	store := newFakeStore("exec-1")
	store.authErr = executors.ErrDisabled

	resp := helloExchange(t, store, protocol.ExecutorHelloRequest{Credential: "c"})

	if resp.Retryable {
		t.Error("a revoked row is terminal, not retryable")
	}
	if !strings.Contains(resp.Error, "disabled") {
		t.Errorf("the refusal must name the reason: %q", resp.Error)
	}
}

// An error nobody classified is assumed transient. Quitting on a genuinely
// dead credential costs a log line; quitting on a transient one costs the
// fleet.
func TestUnclassifiedAuthErrorsAreTreatedAsRetryable(t *testing.T) {
	store := newFakeStore("exec-1")
	store.authErr = errors.New("something nobody anticipated")

	resp := helloExchange(t, store, protocol.ExecutorHelloRequest{Credential: "c"})

	if !resp.Retryable {
		t.Error("an unclassified failure must fail toward retry, not toward exit")
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

// servePool stands up a real TLS listener running p.Serve and returns its
// address, the certificate fingerprint to pin, and the Pool itself — for
// tests that need to reach into the pool (e.g. to mint a ticket) rather than
// only dial it.
func servePool(t *testing.T, store executors.Store) (addr, pin string, p *Pool) {
	t.Helper()

	// Fast reconnects so the retry loop is exercised in milliseconds.
	oldInitial, oldMax := initialBackoff, maxBackoff
	initialBackoff, maxBackoff = 20*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { initialBackoff, maxBackoff = oldInitial, oldMax })

	cert := testCert(t)
	sum := sha256.Sum256(cert.Certificate[0])

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   ALPNProtocols,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	p = New(store)
	go func() { _ = p.Serve(ln) }()

	return ln.Addr().String(), fmt.Sprintf("%x", sum[:]), p
}

func connectOpts(t *testing.T, addr, pin string) ConnectOptions {
	t.Helper()
	credFile := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credFile, []byte("a-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, handler := executorpbconnect.NewExecutorServiceHandler(&stubHandler{executorID: "exec-1"})
	return ConnectOptions{
		Addr:           addr,
		ServerName:     "localhost",
		PinCert:        pin,
		CredentialFile: credFile,
		Handler:        handler,
	}
}

// helloExchange drives one hello frame through handleConn and returns the
// response, without the reconnect loop in the way.
func helloExchange(t *testing.T, store executors.Store, req protocol.ExecutorHelloRequest) protocol.ExecutorHelloResponse {
	t.Helper()
	ours, theirs := net.Pipe()
	t.Cleanup(func() { _ = ours.Close(); _ = theirs.Close() })

	p := New(store)
	go p.handleConn(theirs)

	sendHelloFrame(t, ours, req)

	_ = ours.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := readHelloResponseLine(ours)
	if err != nil {
		t.Fatalf("read hello response: %v", err)
	}
	var resp protocol.ExecutorHelloResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("parse hello response %q: %v", line, err)
	}
	return resp
}

// helloExchangeOn is helloExchange against a caller-supplied pool, for tests
// that must mint into the same registry the connection will redeem from.
func helloExchangeOn(t *testing.T, p *Pool, req protocol.ExecutorHelloRequest) protocol.ExecutorHelloResponse {
	t.Helper()
	ours, theirs := net.Pipe()
	t.Cleanup(func() { _ = ours.Close(); _ = theirs.Close() })
	go p.handleConn(theirs)
	sendHelloFrame(t, ours, req)
	_ = ours.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := readHelloResponseLine(ours)
	if err != nil {
		t.Fatalf("read hello response: %v", err)
	}
	var resp protocol.ExecutorHelloResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("parse hello response %q: %v", line, err)
	}
	return resp
}

func protocolHello(ticket string) protocol.ExecutorHelloRequest {
	return protocol.ExecutorHelloRequest{Type: "executor_hello", Ticket: ticket}
}
