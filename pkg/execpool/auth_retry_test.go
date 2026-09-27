package execpool

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
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

	_, _, err := upgradeExchange(t, New(store), bearerHeader("c"))
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) || ref.Status != http.StatusServiceUnavailable {
		t.Fatalf("a store failure must be refused 503, got %v", err)
	}
	if reason := ref.Reason; reason != "rafikid could not verify the credential right now; retry" {
		t.Errorf("the 503 body must be the fixed retryable text, got %q", reason)
	}
	for _, leak := range []string{"db.internal", "user=rafiki", "connection refused"} {
		if strings.Contains(ref.Reason, leak) {
			t.Errorf("the response leaked %q to an unauthenticated peer: %s", leak, ref.Reason)
		}
	}
}

// A terminal rejection may say what it is: the text comes from our own
// sentinels, and an operator staring at a machine that will not join needs to
// know it was disabled rather than unreachable.
func TestTerminalRejectionNamesTheReason(t *testing.T) {
	store := newFakeStore("exec-1")
	store.authErr = executors.ErrDisabled

	_, _, err := upgradeExchange(t, New(store), bearerHeader("c"))
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) || ref.Status != http.StatusUnauthorized {
		t.Fatalf("a revoked credential must be refused 401, got %v", err)
	}
	if !strings.Contains(ref.Reason, "disabled") {
		t.Errorf("the refusal must name the reason: %q", ref.Reason)
	}
}

// An error nobody classified is assumed transient. Quitting on a genuinely
// dead credential costs a log line; quitting on a transient one costs the
// fleet.
func TestUnclassifiedAuthErrorsAreTreatedAsRetryable(t *testing.T) {
	store := newFakeStore("exec-1")
	store.authErr = errors.New("something nobody anticipated")

	_, _, err := upgradeExchange(t, New(store), bearerHeader("c"))
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) || ref.Status != http.StatusServiceUnavailable {
		t.Fatalf("an unclassified failure must be refused 503 toward retry, got %v", err)
	}
}

// A peer still speaking the old JSON hello frame sends no Authorization
// header at all. The refusal must tell it that header auth replaced the hello
// frame, so whoever operates the machine knows to upgrade.
func TestUpgradeWithoutAuthorizationNamesTheUpgradeHint(t *testing.T) {
	store := newFakeStore("exec-1")

	_, _, err := upgradeExchange(t, New(store), nil)
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) || ref.Status != http.StatusUnauthorized {
		t.Fatalf("a missing Authorization header must be refused 401, got %v", err)
	}
	if !strings.Contains(ref.Reason, "predates header auth") {
		t.Errorf("the 401 must name the old-client hint, got %q", ref.Reason)
	}
}

// One credential names one row, and the pool holds one connection per row: a
// second connection while the incumbent answers is refused 409, and the
// client classifies that as retryable rather than terminal — the incumbent
// may simply be about to die.
func TestUpgradeAlreadyConnectedIs409AndRetryable(t *testing.T) {
	store := newFakeStore("exec-1")
	addr, pin, p := servePool(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = Connect(ctx, connectOpts(t, addr, pin)) }()
	waitFor(t, 5*time.Second, "the incumbent to join", func() bool { return len(p.Live()) == 1 })

	_, _, err := upgradeExchange(t, p, bearerHeader("a-credential"))
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) || ref.Status != http.StatusConflict {
		t.Fatalf("a second live connection must be refused 409, got %v", err)
	}
	if !strings.Contains(ref.Reason, "already live") {
		t.Errorf("the 409 must say why: %q", ref.Reason)
	}
	if errors.Is(classifyRefusal(err), ErrEnrollmentRejected) {
		t.Error("a 409 is retryable; the client must not classify it as a rejected credential")
	}
}

// Enrollment carries the executor's self-reported capability facts to the
// store, and the 101 answers with the minted credential and the row id.
func TestUpgradeEnrollCarriesSelfReportedToTheStore(t *testing.T) {
	store := newFakeStore("exec-1")

	hdr := http.Header{
		"Authorization":                {"Enroll t_token"},
		upgradeconn.HeaderSelfReported: {"os=linux&arch=arm64"},
	}
	_, resp, err := upgradeExchange(t, New(store), hdr)
	if err != nil {
		t.Fatalf("enrollment upgrade refused: %v", err)
	}
	if got := store.lastEnrollment(); !mapsEqual(got, map[string]string{"os": "linux", "arch": "arm64"}) {
		t.Errorf("Enroll received %v, want the self-reported facts", got)
	}
	if got := resp.Get(upgradeconn.HeaderCredential); got != "credential" {
		t.Errorf("the 101 must carry the minted credential, got %q", got)
	}
	if got := resp.Get(upgradeconn.HeaderExecutorID); got != "exec-1" {
		t.Errorf("the 101 must carry the executor id, got %q", got)
	}
}

// The whole enrollment path, real client against real pool: an Enroll token
// buys a credential on the 101, the client persists it, and the next dial
// presents it as Bearer rather than re-sending the now-spent token. Each side
// is pinned on its own elsewhere; this pins that they agree with each other.
func TestEnrollPersistsTheCredentialAndTheNextDialIsBearer(t *testing.T) {
	store := newFakeStore("exec-1")
	addr, pin, _ := servePool(t, store)

	o := connectOpts(t, addr, pin)
	if err := os.Remove(o.CredentialFile); err != nil {
		t.Fatal(err)
	}
	o.EnrollToken = "t_token"
	o.SelfReported = map[string]string{"os": "linux"}

	runUntil := func(done func() bool) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		exited := make(chan struct{})
		go func() { _ = Connect(ctx, o); close(exited) }()
		deadline := time.Now().Add(5 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				cancel()
				<-exited
				t.Fatal("timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-exited
	}

	runUntil(func() bool {
		b, err := os.ReadFile(o.CredentialFile)
		return err == nil && string(b) == "credential\n"
	})
	if got := store.lastEnrollment(); got["os"] != "linux" {
		t.Errorf("Enroll received %v, want the self-reported facts", got)
	}
	if n := len(store.authCalls); n != 0 {
		t.Fatalf("Authenticate ran %d time(s) during enrollment", n)
	}

	runUntil(func() bool { return len(store.authCalls) > 0 })
}

// A malformed Rafiki-Self-Reported header is a bad request, not an auth
// failure: the credential may be fine, the encoding is not.
func TestUpgradeMalformedSelfReportedIs400(t *testing.T) {
	store := newFakeStore("exec-1")

	hdr := http.Header{
		"Authorization":                {"Enroll t_token"},
		upgradeconn.HeaderSelfReported: {"%zz"},
	}
	_, _, err := upgradeExchange(t, New(store), hdr)
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) || ref.Status != http.StatusBadRequest {
		t.Fatalf("a malformed self-reported header must be refused 400, got %v", err)
	}
}

func bearerHeader(cred string) http.Header {
	return http.Header{"Authorization": {string(upgradeconn.SchemeBearer) + " " + cred}}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
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

// upgradeExchange drives one upgrade request through the pool's own endpoint
// and returns the client half of the upgraded connection together with the
// 101's headers. A refusal comes back as a *upgradeconn.Refused error, so a
// test asserts on Status and Reason without the reconnect loop in the way.
func upgradeExchange(t *testing.T, p *Pool, hdr http.Header) (*upgradeconn.Conn, http.Header, error) {
	t.Helper()
	srv := httptest.NewServer(p.UpgradeHandler())
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return upgradeconn.Dial(conn, upgradeconn.Executor, host, hdr)
}
