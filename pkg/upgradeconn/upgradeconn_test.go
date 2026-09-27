package upgradeconn

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// oldClientBody is the exact 401 body header auth must send to a peer still
// speaking the JSON hello, pinned here so it cannot drift unnoticed.
const oldClientBody = `no credential on the upgrade request: send "Authorization: Bearer|Enroll|Ticket <secret>"; a peer sending a JSON hello frame predates header auth and must be upgraded`

// serveMux stands up an HTTP/1.1 server with upgrade handlers on two paths and
// returns its address. This is the shape the daemon uses: one listener, one
// mux, protocols distinguished by PATH rather than by sniffing bytes.
func serveMux(t *testing.T, handlers map[Protocol]func(*Conn, struct{})) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for proto, fn := range handlers {
		mux.Handle(PathFor(proto), Handler(proto,
			func(*http.Request) (struct{}, http.Header, error) { return struct{}{}, nil, nil },
			fn))
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	return ln.Addr().String()
}

// serveAuth stands up an HTTP/1.1 server with ONE upgrade handler whose
// authorize func is caller-supplied, so a test can drive refusals, 500s and
// 101 headers through it. Returns the server address.
func serveAuth[T any](t *testing.T, proto Protocol,
	authorize func(*http.Request) (T, http.Header, error),
	serve func(*Conn, T),
) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(PathFor(proto), Handler(proto, authorize, serve))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	return ln.Addr().String()
}

func dialTo(t *testing.T, addr string, proto Protocol) *Conn {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := Dial(raw, proto, addr, nil)
	if err != nil {
		t.Fatalf("upgrade %s: %v", proto, err)
	}
	return c
}

// dialHdr is dialTo with a caller-supplied request header, returning the 101's
// headers alongside the upgraded Conn.
func dialHdr(t *testing.T, addr string, proto Protocol, hdr http.Header) (*Conn, http.Header) {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	c, respHdr, err := Dial(raw, proto, addr, hdr)
	if err != nil {
		t.Fatalf("upgrade %s: %v", proto, err)
	}
	return c, respHdr
}

// Executor and Daraja share one port, routed by path — the whole point.
func TestTwoProtocolsShareOneListenerByPath(t *testing.T) {
	got := make(chan string, 2)
	addr := serveMux(t, map[Protocol]func(*Conn, struct{}){
		Executor: func(c *Conn, _ struct{}) {
			defer c.Close()
			line, _ := bufio.NewReader(c).ReadString('\n')
			got <- "executor:" + strings.TrimSpace(line)
		},
		Daraja: func(c *Conn, _ struct{}) {
			defer c.Close()
			line, _ := bufio.NewReader(c).ReadString('\n')
			got <- "daraja:" + strings.TrimSpace(line)
		},
	})

	c1 := dialTo(t, addr, Executor)
	_, _ = c1.Write([]byte("{\"type\":\"executor_hello\"}\n"))
	c2 := dialTo(t, addr, Daraja)
	_, _ = c2.Write([]byte("{\"type\":\"daraja_hello\"}\n"))

	seen := map[string]bool{}
	for range 2 {
		select {
		case s := <-got:
			seen[s] = true
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for both handlers")
		}
	}
	if !seen[`executor:{"type":"executor_hello"}`] {
		t.Errorf("executor handler did not receive its frame; saw %v", seen)
	}
	if !seen[`daraja:{"type":"daraja_hello"}`] {
		t.Errorf("daraja handler did not receive its frame; saw %v", seen)
	}
}

// THE hazard. A client routinely writes its hello frame and its first
// payload in ONE segment, so by the time the HTTP server has finished parsing
// the upgrade request those extra bytes are already in the hijack buffer — not
// on the socket. A handler that reads the raw net.Conn loses them and the
// client hangs to its timeout.
//
// Re-run through the auth-bearing signature with a NON-EMPTY 101 header: the
// extra header lines must not disturb the buffering the pipelined frames sit
// in.
func TestUpgradeAuthBytesPipelinedBehindThe101AreNotLost(t *testing.T) {
	lines := make(chan string, 4)
	addr := serveAuth(t, Daraja,
		func(*http.Request) (struct{}, http.Header, error) {
			return struct{}{}, http.Header{HeaderCredential: []string{"c1"}}, nil
		},
		func(c *Conn, _ struct{}) {
			defer c.Close()
			br := bufio.NewReader(c)
			for range 2 {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				lines <- strings.TrimSpace(line)
			}
		})

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	// One write: the upgrade request AND both frames. This is what makes the
	// bytes land in the hijack buffer rather than on the socket.
	req := "GET " + PathFor(Daraja) + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: " + string(Daraja) + "\r\n" +
		"Connection: Upgrade\r\n\r\n" +
		"{\"type\":\"daraja_hello\"}\n" +
		"{\"type\":\"daraja_more\"}\n"
	if _, err := raw.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	// Drain the 101 — asserting the extra header rode it — so the test client
	// is positioned correctly.
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read 101: %v", err)
	}
	if got := resp.Header.Get(HeaderCredential); got != "c1" {
		t.Errorf("101 carried %s %q, want %q", HeaderCredential, got, "c1")
	}

	for _, want := range []string{`{"type":"daraja_hello"}`, `{"type":"daraja_more"}`} {
		select {
		case got := <-lines:
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q — a pipelined frame was dropped with the hijack buffer", want)
		}
	}
}

// The mirror hazard. The executor writes a hello frame and then immediately
// starts speaking HTTP/2, so anything that reads the hello with a throwaway
// buffered reader takes the client preface with it and the connection dies
// mid-frame. Reading everything through the one Conn keeps them in order.
func TestAStreamFollowingTheFirstFrameSurvives(t *testing.T) {
	done := make(chan string, 1)
	addr := serveMux(t, map[Protocol]func(*Conn, struct{}){
		Executor: func(c *Conn, _ struct{}) {
			defer c.Close()
			// Read the hello byte-at-a-time, the way the executor link does.
			var hello []byte
			buf := make([]byte, 1)
			for {
				if _, err := c.Read(buf); err != nil {
					return
				}
				if buf[0] == '\n' {
					break
				}
				hello = append(hello, buf[0])
			}
			// Everything after it must still be there.
			rest, _ := io.ReadAll(c)
			done <- string(hello) + "|" + string(rest)
		},
	})

	c := dialTo(t, addr, Executor)
	if _, err := c.Write([]byte("{\"type\":\"executor_hello\"}\nPRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = c.Conn.(*net.TCPConn).CloseWrite()

	select {
	case got := <-done:
		want := `{"type":"executor_hello"}|PRI * HTTP/2.0` + "\r\n\r\nSM\r\n\r\n"
		if got != want {
			t.Errorf("stream after the first frame was corrupted:\n got %q\nwant %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

// A wrong or missing Upgrade header must fail at the handshake with something a
// human can read, rather than as garbage in the first frame.
func TestAMismatchedUpgradeIsRefusedAtTheHandshake(t *testing.T) {
	addr := serveMux(t, map[Protocol]func(*Conn, struct{}){
		Executor: func(c *Conn, _ struct{}) { c.Close() },
	})

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	// A Daraja request lands on the Executor handler's mux path only if the
	// daemon misroutes; here no handler owns the Daraja path at all, so the
	// mux refuses it with a readable HTTP status before any upgrade.
	if _, _, err := Dial(raw, Daraja, addr, nil); err == nil {
		t.Fatal("an upgrade to the wrong protocol was accepted")
	}
}

// authorize runs BEFORE the hijack, so a refusal is an ordinary HTTP response:
// the client sees a real status and body, and serve is never reached.
func TestUpgradeAuthRefusalIsAnOrdinaryResponse(t *testing.T) {
	var served atomic.Bool
	addr := serveAuth(t, Executor,
		func(*http.Request) (struct{}, http.Header, error) {
			return struct{}{}, nil, &Refusal{Status: http.StatusUnauthorized, Reason: "nope"}
		},
		func(*Conn, struct{}) { served.Store(true) })

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	_, _, err = Dial(raw, Executor, addr, nil)
	var ref *Refused
	if !errors.As(err, &ref) {
		t.Fatalf("want *Refused, got %v", err)
	}
	if ref.Status != http.StatusUnauthorized || ref.Reason != "nope" {
		t.Errorf("got %d %q, want 401 %q", ref.Status, ref.Reason, "nope")
	}
	if served.Load() {
		t.Error("serve ran on a refused upgrade — a refusal must never hijack")
	}
}

// An authorize error that is not a *Refusal is a bug in the caller's code: log
// it server-side and answer an opaque 500, never leaking the error text.
func TestUpgradeAuthNonRefusalErrorIs500(t *testing.T) {
	var served atomic.Bool
	addr := serveAuth(t, Executor,
		func(*http.Request) (struct{}, http.Header, error) {
			return struct{}{}, nil, errors.New("boom")
		},
		func(*Conn, struct{}) { served.Store(true) })

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	_, _, err = Dial(raw, Executor, addr, nil)
	var ref *Refused
	if !errors.As(err, &ref) {
		t.Fatalf("want *Refused, got %v", err)
	}
	if ref.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", ref.Status)
	}
	if strings.Contains(ref.Reason, "boom") {
		t.Errorf("reason %q leaks the authorize error; want the opaque body", ref.Reason)
	}
	if served.Load() {
		t.Error("serve ran on a failed authorize")
	}
}

// Headers authorize returns ride the 101 to the client — this is how a minted
// credential reaches the executor in one round trip.
func TestUpgradeAuthHeadersRideThe101(t *testing.T) {
	addr := serveAuth(t, Executor,
		func(*http.Request) (struct{}, http.Header, error) {
			return struct{}{}, http.Header{HeaderCredential: []string{"c1"}}, nil
		},
		func(c *Conn, _ struct{}) { defer c.Close() })

	_, hdr := dialHdr(t, addr, Executor, nil)
	if got := hdr.Get(HeaderCredential); got != "c1" {
		t.Errorf("101 carried %s %q, want %q", HeaderCredential, got, "c1")
	}
}

// The authorize func sees the request's headers — e.g. Authorization — and the
// value it returns reaches serve as T, the auth context the link then uses.
func TestUpgradeAuthSeesRequestHeaders(t *testing.T) {
	seen := make(chan string, 1)
	addr := serveAuth(t, Executor,
		func(r *http.Request) (string, http.Header, error) {
			scheme, secret, ref := AuthorizationFrom(r)
			if ref != nil {
				return "", nil, ref
			}
			if scheme != SchemeTicket {
				return "", nil, fmt.Errorf("want scheme %q, got %q", SchemeTicket, scheme)
			}
			return secret, nil, nil
		},
		func(c *Conn, secret string) {
			defer c.Close()
			seen <- secret
		})

	if _, hdr := dialHdr(t, addr, Executor, http.Header{"Authorization": {"Ticket t1"}}); hdr.Get(HeaderCredential) != "" {
		t.Error("no credential was minted; the 101 must not carry one")
	}

	select {
	case got := <-seen:
		if got != "t1" {
			t.Errorf("serve got %q, want the secret authorize returned (\"t1\")", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for serve")
	}
}

// A mismatched Upgrade header must be refused by the handler's own check,
// BEFORE authorize — auth is not consulted for a request the endpoint cannot
// serve.
func TestUpgradeAuthNotCalledOnMismatchedUpgrade(t *testing.T) {
	var authorized atomic.Bool
	// The Executor handler is mounted where a Daraja dial lands, so the
	// request REACHES the handler and the refusal comes from the Upgrade
	// check rather than the mux's 404.
	mux := http.NewServeMux()
	mux.Handle(PathFor(Daraja), Handler(Executor,
		func(*http.Request) (struct{}, http.Header, error) {
			authorized.Store(true)
			return struct{}{}, nil, nil
		},
		func(*Conn, struct{}) {}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	addr := ln.Addr().String()

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	_, _, err = Dial(raw, Daraja, addr, nil)
	var ref *Refused
	if !errors.As(err, &ref) {
		t.Fatalf("want *Refused, got %v", err)
	}
	if ref.Status != http.StatusUpgradeRequired {
		t.Errorf("status = %d, want 426", ref.Status)
	}
	if authorized.Load() {
		t.Error("authorize ran on a mismatched upgrade")
	}
}

// AuthorizationFrom must accept exactly the three schemes rafiki speaks,
// canonicalise the scheme, and refuse everything else with the same 401 body
// the pre-header-auth clients can read.
func TestUpgradeAuthorizationFromParses(t *testing.T) {
	cases := []struct {
		name       string
		auth       []string
		wantScheme Scheme
		wantSecret string
		wantRefuse bool
	}{
		{"bearer", []string{"Bearer x"}, SchemeBearer, "x", false},
		{"scheme case-insensitive", []string{"bearer x"}, SchemeBearer, "x", false},
		{"enroll", []string{"Enroll t"}, SchemeEnroll, "t", false},
		{"ticket", []string{"Ticket k"}, SchemeTicket, "k", false},
		{"secret trimmed", []string{"Ticket  k "}, SchemeTicket, "k", false},
		{"missing", nil, "", "", true},
		{"two headers", []string{"Bearer x", "Bearer y"}, "", "", true},
		{"schemeless", []string{"Bearer"}, "", "", true},
		{"empty secret", []string{"Bearer   "}, "", "", true},
		{"unknown scheme", []string{"Basic x"}, "", "", true},
		{"no scheme at all", []string{"x"}, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}}
			for _, v := range tc.auth {
				r.Header.Add("Authorization", v)
			}
			scheme, secret, ref := AuthorizationFrom(r)
			if !tc.wantRefuse {
				if ref != nil {
					t.Fatalf("unexpected refusal: %v", ref)
				}
				if scheme != tc.wantScheme {
					t.Errorf("scheme = %q, want canonical %q", scheme, tc.wantScheme)
				}
				if secret != tc.wantSecret {
					t.Errorf("secret = %q, want %q", secret, tc.wantSecret)
				}
				return
			}
			if ref == nil {
				t.Fatal("want a refusal")
			}
			if ref.Status != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", ref.Status)
			}
			if ref.Reason != oldClientBody {
				t.Errorf("reason =\n%q\nwant the exact old-client body\n%q", ref.Reason, oldClientBody)
			}
		})
	}
}

// Close must be safe from both directions: the handler owns the connection, and
// a caller may also close it on teardown.
func TestConcurrentCloseIsSafe(t *testing.T) {
	addr := serveMux(t, map[Protocol]func(*Conn, struct{}){
		Executor: func(c *Conn, _ struct{}) { _ = c.Close() },
	})
	c := dialTo(t, addr, Executor)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.Close() }()
	}
	wg.Wait()
}

// Each protocol needs its own path, or two diallers reach the same handler and
// the mismatch surfaces as garbage in the first frame rather than as a readable
// HTTP status — which is the whole reason this indirection exists.
func TestEveryProtocolHasItsOwnPath(t *testing.T) {
	seen := map[string]Protocol{}
	for _, p := range []Protocol{Executor, Daraja} {
		path := PathFor(p)
		if path == "/" {
			t.Errorf("PathFor(%q) fell through to the default", p)
		}
		if prev, dup := seen[path]; dup {
			t.Errorf("PathFor(%q) == PathFor(%q) == %q", p, prev, path)
		}
		seen[path] = p
	}
}
