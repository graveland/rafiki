package main

// mustDial's token-carrying dial: a profile with a token sends ctrl_auth as
// the framed socket's first frame; a profile without one sends nothing. This
// is the client half of the UDS optional-auth handshake (pkg/control's
// ListenWithAuth) — without it, framed verbs ran anonymous while Connect verbs
// ran as the profile's user, and owner-scoped state (presets) was invisible to
// `rafiki create --preset` on the same profile.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// recordingDaemon accepts framed connections and records the type, id and
// token of each connection's FIRST frame, then answers every non-auth frame
// with a success ctrl_response echoing the request. (The auth frame is
// consumed by the server's handshake, so it must never be answered.)
type recordingDaemon struct {
	mu     sync.Mutex
	firsts []string // "type id token" per connection
}

func (d *recordingDaemon) serve(t *testing.T, sockPath string) {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serveConn(conn)
		}
	}()
}

func (d *recordingDaemon) serveConn(conn net.Conn) {
	defer conn.Close()
	r := protocol.NewFrameReader(conn, 1<<20)
	sawFirst := false
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
		if json.Unmarshal(frame, &req) != nil {
			continue
		}
		if !sawFirst {
			sawFirst = true
			d.mu.Lock()
			d.firsts = append(d.firsts, req.Type+" "+req.ID+" "+req.Token)
			d.mu.Unlock()
		}
		if req.Type == protocol.TypeCtrlAuth {
			continue // handshake frame, not a request
		}
		payload, _ := json.Marshal(req)
		resp, err := json.Marshal(protocol.Response{
			Type: protocol.TypeCtrlResponse, Command: req.Type, ID: req.ID,
			Success: true, Data: payload,
		})
		if err != nil {
			return
		}
		_ = protocol.WriteFrame(conn, resp)
	}
}

// snapshot returns a copy of the recorded first frames.
func (d *recordingDaemon) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.firsts...)
}

// writeTokenedProfile isolates the profile environment and writes one profile
// "it" at sockPath, with a token file when token != "".
func writeTokenedProfile(t *testing.T, sockPath, token string) {
	t.Helper()
	set := profile.Set{Profiles: map[string]profile.Profile{
		"it": {Name: "it", Socket: sockPath},
	}}
	if err := profile.Save(set); err != nil {
		t.Fatalf("save profile: %v", err)
	}
	if err := profile.SavePointer("it"); err != nil {
		t.Fatalf("save pointer: %v", err)
	}
	if token != "" {
		if err := profile.WriteToken("it", token); err != nil {
			t.Fatalf("write token: %v", err)
		}
	}
}

// dialAndRequest runs mustDial against the profile and issues one request, so
// the fake daemon's first-frame record is settled by the time the test reads it.
func dialAndRequest(t *testing.T, sockPath string) {
	t.Helper()
	c := mustDial(&cobra.Command{})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Request(ctx, protocol.StatusRequest{Type: protocol.TypeCtrlStatus, ID: "req-1"})
	if err != nil || !resp.Success {
		t.Fatalf("request over the profile dial failed: %v / %+v", err, resp)
	}
}

// firstFrame waits for the fake's first-frame record of the (single) connection.
func firstFrame(t *testing.T, d *recordingDaemon) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := d.snapshot(); len(got) > 0 {
			return got[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the fake daemon never recorded a first frame")
	return ""
}

func TestMustDialSendsTheProfileTokenOnTheSocket(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: macOS caps UDS paths at 104 bytes and the
	// test name pushes t.TempDir over it.
	dir, err := os.MkdirTemp("", "rafiki")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "controller.sock")
	d := &recordingDaemon{}
	d.serve(t, sockPath)
	writeTokenedProfile(t, sockPath, "rfk_tok")

	dialAndRequest(t, sockPath)

	got := firstFrame(t, d)
	if got != "ctrl_auth 0 rfk_tok" {
		t.Fatalf("first frame = %q, want the ctrl_auth handshake frame carrying the profile token", got)
	}
}

func TestMustDialWithoutATokenSendsNoAuthFrame(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: macOS caps UDS paths at 104 bytes and the
	// test name pushes t.TempDir over it.
	dir, err := os.MkdirTemp("", "rafiki")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "controller.sock")
	d := &recordingDaemon{}
	d.serve(t, sockPath)
	writeTokenedProfile(t, sockPath, "")

	dialAndRequest(t, sockPath)

	got := firstFrame(t, d)
	if got != "ctrl_status req-1 " {
		t.Fatalf("first frame = %q, want the request itself (no ctrl_auth frame)", got)
	}
}

// The token file is the credential; a manifest without one must not conjure a
// handshake. (writeTokenedProfile with token="" covers it; this explicit test
// documents that the token FILE, not any manifest field, drives the dial.)
func TestMustDialTokenComesFromTheTokenFile(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: macOS caps UDS paths at 104 bytes and the
	// test name pushes t.TempDir over it.
	dir, err := os.MkdirTemp("", "rafiki")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "controller.sock")
	d := &recordingDaemon{}
	d.serve(t, sockPath)
	writeTokenedProfile(t, sockPath, "rfk_tok")
	// Overwrite the token file with different content: the FILE wins.
	if err := profile.WriteToken("it", "rfk_other"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profile.TokenFile("it")); err != nil {
		t.Fatalf("token file missing: %v", err)
	}

	dialAndRequest(t, sockPath)

	got := firstFrame(t, d)
	if got != "ctrl_auth 0 rfk_other" {
		t.Fatalf("first frame = %q, want the token from the token file", got)
	}
}
