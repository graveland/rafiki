package execpool

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stdioPair builds the two halves of a bidirectional connection out of two
// io.Pipes, wired exactly as production wires them: one side's writer is the
// other side's reader. That is the pairing `docker exec -i` produces, and this
// gets it without docker or a subprocess.
func stdioPair() (server, client net.Conn) {
	toServerR, toServerW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	return NewStdioConn(toServerR, toClientW), NewStdioConn(toClientR, toServerW)
}

// The whole point of stdioConn is that ServeInverted and ClientForConn work over
// it unchanged. If HTTP/2 needed connection deadlines this would hang or error
// rather than complete, so this test is also what backs the deadline analysis in
// stdioconn.go: it drives a real h2 exchange, prologue and all.
func TestHTTP2RoundTripOverStdioConn(t *testing.T) {
	serverConn, clientConn := stdioPair()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("echo:" + string(body)))
	})

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = ServeInverted(serverConn, handler)
	}()

	client, err := ClientForConn(clientConn)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Post("http://stdio/echo", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("round trip over stdio failed: %v", err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	if string(got) != "echo:hello" {
		t.Fatalf("got %q, want %q", got, "echo:hello")
	}
	if resp.ProtoMajor != 2 {
		t.Errorf("expected HTTP/2, got HTTP/%d.%d", resp.ProtoMajor, resp.ProtoMinor)
	}

	// A second request on the same connection. The transport hands its
	// connection over exactly once (a second dial is ErrRedialed), so this
	// proves the first exchange neither consumed nor desynced it.
	resp2, err := client.Post("http://stdio/echo", "text/plain", strings.NewReader("again"))
	if err != nil {
		t.Fatalf("second round trip failed (connection desynced?): %v", err)
	}
	defer resp2.Body.Close()
	got2, _ := io.ReadAll(resp2.Body)
	if string(got2) != "echo:again" {
		t.Fatalf("second response: got %q", got2)
	}

	if err := clientConn.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Error("ServeInverted did not return after the connection closed")
	}
}

// Teardown arrives from two directions — the transport giving up, and Release
// killing the workspace — so a second Close must not report a failure the
// caller will log as a real one.
func TestStdioConnCloseIsIdempotent(t *testing.T) {
	_, client := stdioPair()
	first := client.Close()
	second := client.Close()
	if first != second {
		t.Errorf("Close is not idempotent: first=%v second=%v", first, second)
	}
}

// The deadline setters must fail loudly rather than return nil. A nil return
// would claim a bound a pipe pair cannot enforce, and a caller relying on it
// would wait forever with nothing to explain why.
func TestStdioConnRefusesDeadlinesExplicitly(t *testing.T) {
	_, client := stdioPair()
	defer client.Close()
	for name, err := range map[string]error{
		"SetDeadline":      client.SetDeadline(time.Now()),
		"SetReadDeadline":  client.SetReadDeadline(time.Now()),
		"SetWriteDeadline": client.SetWriteDeadline(time.Now()),
	} {
		if !errors.Is(err, ErrNoDeadline) {
			t.Errorf("%s returned %v, want ErrNoDeadline", name, err)
		}
	}
}
