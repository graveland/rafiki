package upgradeconn

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// serveMux stands up an HTTP/1.1 server with upgrade handlers on two paths and
// returns its address. This is the shape the daemon uses: one listener, one
// mux, protocols distinguished by PATH rather than by sniffing bytes.
func serveMux(t *testing.T, handlers map[Protocol]func(*Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for proto, fn := range handlers {
		mux.Handle(PathFor(proto), Handler(proto, fn))
	}
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
	c, err := Dial(raw, proto, addr)
	if err != nil {
		t.Fatalf("upgrade %s: %v", proto, err)
	}
	return c
}

// Two protocols, one port, routed by path — the whole point.
func TestTwoProtocolsShareOneListenerByPath(t *testing.T) {
	got := make(chan string, 2)
	addr := serveMux(t, map[Protocol]func(*Conn){
		Control: func(c *Conn) {
			defer c.Close()
			line, _ := bufio.NewReader(c).ReadString('\n')
			got <- "control:" + strings.TrimSpace(line)
		},
		Executor: func(c *Conn) {
			defer c.Close()
			line, _ := bufio.NewReader(c).ReadString('\n')
			got <- "executor:" + strings.TrimSpace(line)
		},
	})

	c1 := dialTo(t, addr, Control)
	_, _ = c1.Write([]byte("{\"type\":\"ctrl_auth\"}\n"))
	c2 := dialTo(t, addr, Executor)
	_, _ = c2.Write([]byte("{\"type\":\"executor_hello\"}\n"))

	seen := map[string]bool{}
	for range 2 {
		select {
		case s := <-got:
			seen[s] = true
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for both handlers")
		}
	}
	if !seen[`control:{"type":"ctrl_auth"}`] {
		t.Errorf("control handler did not receive its frame; saw %v", seen)
	}
	if !seen[`executor:{"type":"executor_hello"}`] {
		t.Errorf("executor handler did not receive its frame; saw %v", seen)
	}
}

// THE hazard. A control client routinely writes its auth frame and its first
// request in ONE segment, so by the time the HTTP server has finished parsing
// the upgrade request those extra bytes are already in the hijack buffer — not
// on the socket. A handler that reads the raw net.Conn loses them and the
// client hangs to its timeout.
//
// This is the same bug the control plane already hit once, relocated: there it
// was a second FrameReader discarding the first one's buffer.
func TestBytesPipelinedBehindTheUpgradeAreNotLost(t *testing.T) {
	lines := make(chan string, 4)
	addr := serveMux(t, map[Protocol]func(*Conn){
		Control: func(c *Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			for range 2 {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				lines <- strings.TrimSpace(line)
			}
		},
	})

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	// One write: the upgrade request AND both frames. This is what makes the
	// bytes land in the hijack buffer rather than on the socket.
	req := "GET " + PathFor(Control) + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: " + string(Control) + "\r\n" +
		"Connection: Upgrade\r\n\r\n" +
		"{\"type\":\"ctrl_auth\"}\n" +
		"{\"type\":\"ctrl_list\"}\n"
	if _, err := raw.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	// Drain the 101 so the test client is positioned correctly.
	br := bufio.NewReader(raw)
	if _, err := http.ReadResponse(br, nil); err != nil {
		t.Fatalf("read 101: %v", err)
	}

	for _, want := range []string{`{"type":"ctrl_auth"}`, `{"type":"ctrl_list"}`} {
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
	addr := serveMux(t, map[Protocol]func(*Conn){
		Executor: func(c *Conn) {
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
	addr := serveMux(t, map[Protocol]func(*Conn){
		Executor: func(c *Conn) { c.Close() },
	})

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	// Right path, wrong protocol.
	if _, err := Dial(raw, Control, addr); err == nil {
		t.Fatal("an upgrade to the wrong protocol was accepted")
	}
}

// Close must be safe from both directions: the handler owns the connection, and
// a caller may also close it on teardown.
func TestConcurrentCloseIsSafe(t *testing.T) {
	addr := serveMux(t, map[Protocol]func(*Conn){
		Control: func(c *Conn) { _ = c.Close() },
	})
	c := dialTo(t, addr, Control)
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
	for _, p := range []Protocol{Control, Executor, Daraja} {
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
