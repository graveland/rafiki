package client_test

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

// startEchoServer spins up a tiny UDS that echoes every received frame
// back wrapped in a ctrl_response. Returns the socket path + a cleanup.
func startEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := protocol.NewFrameReader(conn, 1<<20)
		for {
			frame, err := r.ReadFrame()
			if err != nil {
				return
			}
			// Echo as ctrl_response with the original frame as data.
			var req struct{ Type, ID string }
			_ = json.Unmarshal(frame, &req)
			resp := map[string]any{
				"type":    "ctrl_response",
				"command": req.Type,
				"id":      req.ID,
				"success": true,
				"data":    json.RawMessage(frame),
			}
			b, _ := json.Marshal(resp)
			_ = protocol.WriteFrame(conn, b)
		}
	}()

	return path, func() {
		ln.Close()
		<-done
	}
}

func TestClient_DialAndRequest(t *testing.T) {
	path, cleanup := startEchoServer(t)
	defer cleanup()

	c, err := client.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := c.Request(ctx, protocol.StatusRequest{
		Type: protocol.TypeCtrlStatus,
		ID:   "req-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "req-1" {
		t.Fatalf("ID echo: got %q, want %q", resp.ID, "req-1")
	}
	if !resp.Success {
		t.Fatalf("Success: got false")
	}
	if resp.Command != protocol.TypeCtrlStatus {
		t.Fatalf("Command: got %q, want %q", resp.Command, protocol.TypeCtrlStatus)
	}
}

func TestClient_DialFailureReturnsError(t *testing.T) {
	_, err := client.Dial("/nonexistent/path/to/socket")
	if err == nil {
		t.Fatal("expected error dialing nonexistent socket")
	}
}

func TestClient_ContextCancel(t *testing.T) {
	path, cleanup := startEchoServer(t)
	defer cleanup()

	c, err := client.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Cancel before request.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = c.Request(ctx, protocol.StatusRequest{Type: protocol.TypeCtrlStatus})
	if err == nil || !isContextErr(err) {
		t.Fatalf("expected context error, got %v", err)
	}
}

func isContextErr(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
