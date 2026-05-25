package server_test

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"graveland.dev/pi-controller/internal/server"
)

func TestServer_AcceptsAndEchoes(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	handler := func(frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	}

	srv, err := server.Listen(sockPath, handler)
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

	srv1, err := server.Listen(sockPath, func([]byte) []byte { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer srv1.Close()

	// Second listen on same path should fail (live socket).
	_, err = server.Listen(sockPath, func([]byte) []byte { return nil })
	if err == nil {
		t.Fatal("expected error for live socket, got nil")
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Fatalf("expected 'in use' error, got %v", err)
	}
}

func TestServer_StaleSocketUnlinked(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	// First listener.
	srv1, err := server.Listen(sockPath, func([]byte) []byte { return nil })
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

	srv2, err := server.Listen(sockPath, func([]byte) []byte { return nil })
	if err != nil {
		t.Fatalf("recover from stale: %v", err)
	}
	srv2.Close()
}
