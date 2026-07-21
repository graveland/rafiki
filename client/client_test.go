package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.graveland.dev/brent/fundi/client"
	"git.graveland.dev/brent/fundi/protocol"
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

func TestClient_Subscribe_ReceivesEvents(t *testing.T) {
	dir, err := os.MkdirTemp("", "pictest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "t.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
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
			var hdr struct{ Type, ID string }
			_ = json.Unmarshal(frame, &hdr)
			if hdr.Type == protocol.TypeCtrlSubscribe {
				// Ack the subscribe.
				ack := fmt.Sprintf(`{"type":"ctrl_response","command":"ctrl_subscribe","id":%q,"success":true}`, hdr.ID)
				_ = protocol.WriteFrame(conn, []byte(ack))
				// Push 3 events.
				for i := 0; i < 3; i++ {
					ev := fmt.Sprintf(`{"type":"ctrl_event","childId":"c_x","event":{"type":"agent_start","i":%d}}`, i)
					_ = protocol.WriteFrame(conn, []byte(ev))
				}
				return
			}
		}
	}()

	c, err := client.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	events, cancel, err := c.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ctxCancel()

	_, err = c.Request(ctx, protocol.SubscribeRequest{
		Type:    protocol.TypeCtrlSubscribe,
		ChildID: "c_x",
	})
	if err != nil {
		t.Fatal(err)
	}

	got := []json.RawMessage{}
	for i := 0; i < 3; i++ {
		select {
		case ev := <-events:
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
}
