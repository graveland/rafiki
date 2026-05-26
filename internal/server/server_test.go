package server_test

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"graveland.dev/pi-controller/internal/server"
)

func TestServer_AcceptsAndEchoes(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	srv, err := server.Listen(sockPath, server.FuncHandler(func(_ server.Connection, frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	if _, err := conn.Write([]byte(`{"type":"ping"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	got, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got != `echo:{"type":"ping"}`+"\n" {
		t.Fatalf("got %q", got)
	}
}

func TestServer_LiveSocketRejected(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	noHandler := server.FuncHandler(func(server.Connection, []byte) []byte { return nil })

	srv1, err := server.Listen(sockPath, noHandler)
	if err != nil {
		t.Fatal(err)
	}
	defer srv1.Close()

	// Second listen on same path should fail (live socket).
	_, err = server.Listen(sockPath, noHandler)
	if err == nil {
		t.Fatal("expected error for live socket, got nil")
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Fatalf("expected 'in use' error, got %v", err)
	}
}

// TestServer_Broadcast verifies that Broadcast delivers a frame to every
// currently-connected client and returns the correct connection count.
func TestServer_Broadcast(t *testing.T) {
	// Use os.MkdirTemp (not t.TempDir) — macOS UDS paths are capped at 104 bytes
	// and t.TempDir embeds the full test name which can exceed that limit.
	dir, err := os.MkdirTemp("", "pic")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "test.sock")

	noop := server.FuncHandler(func(_ server.Connection, _ []byte) []byte { return nil })
	srv, err := server.Listen(sockPath, noop)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	const numClients = 3
	clients := make([]net.Conn, numClients)
	for i := range clients {
		c, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("dial client %d: %v", i, err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		clients[i] = c
	}

	// Wait briefly for handleConn goroutines to register into the conns map.
	time.Sleep(20 * time.Millisecond)

	sentinel := []byte(`{"type":"ctrl_daemon_shutdown","reason":"test"}`)
	n := srv.Broadcast(sentinel)
	if n != numClients {
		t.Errorf("Broadcast: want %d connections notified, got %d", numClients, n)
	}

	// Every client should receive the sentinel frame (WriteFrame adds '\n').
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func(idx int, conn net.Conn) {
			defer wg.Done()
			br := bufio.NewReader(conn)
			line, err := br.ReadString('\n')
			if err != nil {
				t.Errorf("client %d read: %v", idx, err)
				return
			}
			got := strings.TrimRight(line, "\n")
			if got != string(sentinel) {
				t.Errorf("client %d: got %q, want %q", idx, got, string(sentinel))
			}
		}(i, c)
	}
	wg.Wait()
}

// TestServer_BroadcastAfterClose_Safe verifies that Broadcast after Close
// does not panic and returns 0 (all connections deregistered by wg.Wait).
func TestServer_BroadcastAfterClose_Safe(t *testing.T) {
	dir, err := os.MkdirTemp("", "pic")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "test.sock")

	noop := server.FuncHandler(func(_ server.Connection, _ []byte) []byte { return nil })
	srv, err := server.Listen(sockPath, noop)
	if err != nil {
		t.Fatal(err)
	}
	// Close blocks until all goroutines exit, so conns map is empty afterward.
	srv.Close()

	n := srv.Broadcast([]byte(`{"type":"ctrl_daemon_shutdown"}`))
	if n != 0 {
		t.Errorf("Broadcast after Close: want 0, got %d", n)
	}
}

func TestServer_StaleSocketUnlinked(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	noopHandler := server.FuncHandler(func(server.Connection, []byte) []byte { return nil })

	// First listener.
	srv1, err := server.Listen(sockPath, noopHandler)
	if err != nil {
		t.Fatal(err)
	}
	srv1.Close() // closes listener; socket file remains (OS does not auto-unlink)

	// Place a stray file at the path to simulate a leftover socket.
	// (srv1.Close leaves the socket file; write a regular file on top to
	// ensure we're testing recovery from a non-socket stale file as well.)
	if err := os.WriteFile(sockPath, nil, 0o600); err != nil {
		// Ignore — the socket file from srv1 is still at the path, which is
		// sufficient for the probe to fire.
		_ = err
	}

	srv2, err := server.Listen(sockPath, noopHandler)
	if err != nil {
		t.Fatalf("recover from stale: %v", err)
	}
	srv2.Close()
}
