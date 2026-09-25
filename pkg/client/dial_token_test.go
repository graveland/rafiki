package client_test

// DialWithToken: the token-carrying UDS dial (client.DialWithToken) that makes
// framed verbs run as the profile's identity, matching what Connect verbs do
// over connect.sock with the same profile. The server-side contract it depends
// on — ctrl_auth as an OPTIONAL first frame on the unix socket — is pinned in
// pkg/control's server_uds_auth_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// startRecordingServer starts a tiny UDS that answers framed requests with a
// success ctrl_response echoing the request, and reports the type+token of the
// connection's FIRST frame through first — or the read error, if the server
// never got one. The auth frame must never be answered as a request, so the
// caller can tell "auth frame consumed, request answered" from "auth frame
// dispatched as the request" by which id comes back.
func startRecordingServer(t *testing.T, first chan<- string) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rafiki-client")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
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
		sawAuth := false
		for {
			frame, err := r.ReadFrame()
			if err != nil {
				return
			}
			var req struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Token string `json:"token"`
			}
			_ = json.Unmarshal(frame, &req)
			if req.Type == "ctrl_auth" && !sawAuth {
				sawAuth = true
				select {
				case first <- "ctrl_auth:" + req.Token:
				default:
				}
				continue
			}
			if !sawAuth {
				select {
				case first <- req.Type:
				default:
				}
			}
			resp := map[string]any{
				"type": "ctrl_response", "command": req.Type,
				"id": req.ID, "success": true, "data": json.RawMessage(frame),
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

func requestEcho(t *testing.T, path, token string) *protocol.Response {
	t.Helper()
	var c *client.Client
	var err error
	if token == "" {
		c, err = client.Dial(path)
	} else {
		c, err = client.DialWithToken(path, token)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Request(ctx, protocol.StatusRequest{Type: protocol.TypeCtrlStatus, ID: "req-1"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp
}

// A profile with a token: ctrl_auth goes first, carries the token, is consumed
// by the server's handshake (never dispatched as a request), and the request
// that follows is answered normally.
func TestDialWithToken_SendsCtrlAuthFirst(t *testing.T) {
	first := make(chan string, 1)
	path, cleanup := startRecordingServer(t, first)
	defer cleanup()

	resp := requestEcho(t, path, "rfk_tok")
	if resp.ID != "req-1" || !resp.Success {
		t.Fatalf("response = %+v, want success for id req-1", resp)
	}
	if got := <-first; got != "ctrl_auth:rfk_tok" {
		t.Fatalf("first frame = %q, want ctrl_auth carrying the profile token", got)
	}
}

// A profile without a token dials exactly as before: no ctrl_auth frame, the
// request itself is the first frame.
func TestDialWithToken_EmptyTokenSendsNoAuthFrame(t *testing.T) {
	first := make(chan string, 1)
	path, cleanup := startRecordingServer(t, first)
	defer cleanup()

	resp := requestEcho(t, path, "")
	if resp.ID != "req-1" || !resp.Success {
		t.Fatalf("response = %+v, want success for id req-1", resp)
	}
	select {
	case got := <-first:
		if got != "ctrl_status" {
			t.Fatalf("first frame = %q, want the request itself", got)
		}
	default:
		// The channel holds only the first NON-auth frame's type; nothing to
		// assert beyond the response above.
	}
}

// A refused ctrl_auth is visible to the caller, not silently swallowed: the
// server answers the auth frame with an error and closes, and the next Request
// reports the auth reason rather than a bare "connection closed".
func TestDialWithToken_RejectedAuthSurfacesTheReason(t *testing.T) {
	// os.MkdirTemp, not t.TempDir: macOS caps UDS paths at 104 bytes and the
	// test name pushes t.TempDir over it (same reason TestServer_Broadcast
	// does this).
	dir, err := os.MkdirTemp("", "rafiki")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := protocol.NewFrameReader(conn, 1<<20)
		frame, err := r.ReadFrame()
		if err != nil {
			return
		}
		var req struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(frame, &req) != nil || req.Type != "ctrl_auth" {
			return
		}
		// The server's refusal shape (writeAuthError): an error ctrl_response
		// for the auth frame, then close.
		b, _ := json.Marshal(map[string]any{
			"type": "ctrl_response", "command": "ctrl_auth", "id": "0",
			"success": false, "error": map[string]string{
				"code": "auth_invalid", "message": "invalid auth token",
			},
		})
		_ = protocol.WriteFrame(conn, b)
	}()

	c, err := client.DialWithToken(path, "rfk_stale")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = c.Request(ctx, protocol.StatusRequest{Type: protocol.TypeCtrlStatus})
	if err == nil {
		t.Fatal("a refused ctrl_auth produced no error")
	}
	if !strings.Contains(err.Error(), "auth_invalid") {
		t.Fatalf("error = %v, want the auth reason", err)
	}
}

// Dial (no token) still dials a nonexistent socket as a plain error.
func TestDialWithToken_DialFailureReturnsError(t *testing.T) {
	_, err := client.DialWithToken("/nonexistent/path/to/socket", "rfk_tok")
	if err == nil {
		t.Fatal("expected error dialing nonexistent socket")
	}
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "dial") {
		t.Fatalf("error = %v, want a dial error", err)
	}
}
